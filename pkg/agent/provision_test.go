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

package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// seedTestHarnessConfig creates a minimal harness-config directory for testing.
// Creates <scionDir>/harness-configs/<name>/config.yaml with the given harness type.
func seedTestHarnessConfig(t *testing.T, scionDir, name, harnessType string) {
	t.Helper()
	hcDir := filepath.Join(scionDir, "harness-configs", name)
	os.MkdirAll(hcDir, 0755)
	configYAML := "harness: " + harnessType + "\nimage: test-image:latest\n"
	if harnessType == "claude" {
		configYAML += "skills_dir: .claude/skills\ninstructions_file: .claude/CLAUDE.md\nsystem_prompt_file: .claude/system-prompt.md\n"
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatalf("failed to write harness-config: %v", err)
	}
}

func TestProvisionAgentEnvMerging(t *testing.T) {
	tmpDir := t.TempDir()

	// Move to tmpDir to avoid being inside the project's git repo
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Mock HOME for global settings and templates
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)

	// Create a harness-config for test-harness
	seedTestHarnessConfig(t, globalScionDir, "test-harness", "test-harness")

	// Create an agnostic template (no harness field, uses default_harness_config)
	tplDir := filepath.Join(globalTemplatesDir, "test-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "test-harness",
		"env": {
			"TPL_VAR": "tpl-val",
			"OVERRIDE_VAR": "tpl-override"
		}
	}`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	// Global settings with harness_configs
	globalSettings := `schema_version: "1"
harness_configs:
  test-harness:
    harness: test-harness
    env:
      GLOBAL_VAR: global-val
      OVERRIDE_VAR: global-override
`
	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644)

	// Project settings
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)
	projectSettings := `schema_version: "1"
profiles:
  test-profile:
    runtime: docker
    env:
      PROJECT_VAR: project-val
      OVERRIDE_VAR: project-override
`
	os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(projectSettings), 0644)

	// Provision agent
	agentName := "test-agent"
	_, _, cfg, err := ProvisionAgent(context.Background(), agentName, "test-tpl", "", "", projectScionDir, "test-profile", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Priority (user requested): Global (lowest) -> Project -> Template (highest)
	// So OVERRIDE_VAR should be "tpl-override"

	expectedEnv := map[string]string{
		"GLOBAL_VAR":   "global-val",
		"PROJECT_VAR":  "project-val",
		"TPL_VAR":      "tpl-val",
		"OVERRIDE_VAR": "tpl-override",
	}

	for k, v := range expectedEnv {
		if cfg.Env[k] != v {
			t.Errorf("expected env[%s] = %q, got %q", k, v, cfg.Env[k])
		}
	}

	// Verify it was persisted to scion-agent.json
	agentScionJSON := filepath.Join(projectScionDir, "agents", agentName, "scion-agent.json")
	data, err := os.ReadFile(agentScionJSON)
	if err != nil {
		t.Fatal(err)
	}
	var persistedCfg struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &persistedCfg); err != nil {
		t.Fatal(err)
	}

	for k, v := range expectedEnv {
		if persistedCfg.Env[k] != v {
			t.Errorf("persisted: expected env[%s] = %q, got %q", k, v, persistedCfg.Env[k])
		}
	}
}

func TestProvisionGeminiAgentSettings(t *testing.T) {
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()

	// Move to tmpDir
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Mock HOME
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Seed global harness-configs (required for agent creation)
	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	// Initialize a mock project
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := config.InitProject(projectScionDir, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	// Chdir to projectDir so GetProjectDir finds it
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	// Provision a claude agent using the "default" agnostic template
	agentName := "gemini-agent"
	agentHome, _, _, err := ProvisionAgent(context.Background(), agentName, "default", "", "claude", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify agent's settings.json (copied from claude harness-config's home)
	agentSettingsPath := filepath.Join(agentHome, ".claude", "settings.json")
	data, err := os.ReadFile(agentSettingsPath)
	if err != nil {
		t.Fatalf("failed to read agent settings.json: %v", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to unmarshal agent settings.json: %v", err)
	}

	// With no auth_selected_type in the claude harness config, Provision()
	// should NOT inject a selectedType into settings.json — auth is determined
	// at runtime.
	if security, ok := settings["security"].(map[string]interface{}); ok {
		if auth, ok := security["auth"].(map[string]interface{}); ok {
			if _, ok := auth["selectedType"]; ok {
				t.Error("selectedType should not be set when no auth_selected_type is configured")
			}
		}
	}
}

func TestProvisionWritesTaskToPromptMd(t *testing.T) {
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := config.InitProject(projectScionDir, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	os.Chdir(projectDir)

	rt := &runtime.MockRuntime{}
	mgr := NewManager(rt)

	// Resolve the actual project directory (may be external for non-git projects)
	resolvedProjectDir, _ := config.GetResolvedProjectDir(projectScionDir)

	t.Run("with task", func(t *testing.T) {
		opts := api.StartOptions{
			Name:        "agent-with-task",
			Task:        "implement feature X",
			Template:    "default",
			ProjectPath: projectScionDir,
		}

		_, err := mgr.Provision(context.Background(), opts)
		if err != nil {
			t.Fatalf("Provision failed: %v", err)
		}

		promptFile := filepath.Join(resolvedProjectDir, "agents", "agent-with-task", "prompt.md")
		content, err := os.ReadFile(promptFile)
		if err != nil {
			t.Fatalf("failed to read prompt.md: %v", err)
		}
		if string(content) != "implement feature X" {
			t.Errorf("expected prompt.md to contain 'implement feature X', got %q", string(content))
		}
	})

	t.Run("without task", func(t *testing.T) {
		opts := api.StartOptions{
			Name:        "agent-no-task",
			Template:    "default",
			ProjectPath: projectScionDir,
		}

		_, err := mgr.Provision(context.Background(), opts)
		if err != nil {
			t.Fatalf("Provision failed: %v", err)
		}

		promptFile := filepath.Join(resolvedProjectDir, "agents", "agent-no-task", "prompt.md")
		content, err := os.ReadFile(promptFile)
		if err != nil {
			t.Fatalf("failed to read prompt.md: %v", err)
		}
		if string(content) != "" {
			t.Errorf("expected empty prompt.md, got %q", string(content))
		}
	})
}

func TestProvisionAgentNonGitWorkspace(t *testing.T) {
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()

	// Move to tmpDir to avoid being inside the project's git repo
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Mock HOME
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	// Project-local directory
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := config.InitProject(projectScionDir, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	// Change into projectDir so FindTemplate (via GetProjectDir) finds it
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	evalProjectDir, _ := filepath.EvalSymlinks(projectDir)

	agentName := "test-agent"
	home, ws, cfg, err := ProvisionAgent(context.Background(), agentName, "default", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	if ws != "" {
		t.Errorf("expected empty workspace path for non-git agent, got %q", ws)
	}

	if home == "" {
		t.Error("expected non-empty home path")
	}

	// Check volumes in cfg
	found := false
	for _, v := range cfg.Volumes {
		if v.Target == "/workspace" {
			found = true
			evalSource, _ := filepath.EvalSymlinks(v.Source)
			if evalSource != evalProjectDir {
				t.Errorf("expected volume source %q, got %q", evalProjectDir, evalSource)
			}
		}
	}
	if !found {
		t.Error("expected /workspace volume mount not found in config")
	}

	// Global directory
	if err := config.InitGlobal(getTestHarnesses()); err != nil {
		t.Fatalf("InitGlobal failed: %v", err)
	}
	globalScionDir, _ := config.GetGlobalDir()

	// Change into a subdirectory to act as CWD
	cwd := filepath.Join(tmpDir, "some-dir")
	os.MkdirAll(cwd, 0755)
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	evalCWD, _ := filepath.EvalSymlinks(cwd)

	_, ws, cfg, err = ProvisionAgent(context.Background(), "global-agent", "default", "", "", globalScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed for global project: %v", err)
	}

	if ws != "" {
		t.Errorf("expected empty workspace path for global agent, got %q", ws)
	}

	found = false
	for _, v := range cfg.Volumes {
		if v.Target == "/workspace" {
			found = true
			evalSource, _ := filepath.EvalSymlinks(v.Source)
			if evalSource != evalCWD {
				t.Errorf("expected global agent volume source %q (CWD), got %q", evalCWD, evalSource)
			}
		}
	}
	if !found {
		t.Error("expected /workspace volume mount not found in global agent config")
	}
}

func TestProvisionAgentWorkspaceFlag(t *testing.T) {
	tmpDir := t.TempDir()

	// Move to tmpDir to avoid being inside the project's git repo
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Mock HOME
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)

	// Create a harness-config and agnostic template
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	tplDir := filepath.Join(globalTemplatesDir, "claude")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{"default_harness_config": "claude"}`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	os.MkdirAll(projectDir, 0755)

	// Mock .scion
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)
	os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/"), 0644)

	customWorkspace := filepath.Join(tmpDir, "custom-workspace")
	os.MkdirAll(customWorkspace, 0755)
	evalCustomWorkspace, _ := filepath.EvalSymlinks(customWorkspace)

	// 1. Test valid --workspace in non-git
	agentName := "workspace-agent"
	_, _, cfg, err := ProvisionAgent(context.Background(), agentName, "claude", "", "", projectScionDir, "", "", "", customWorkspace)
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	found := false
	var evalSource string
	for _, v := range cfg.Volumes {
		if v.Target == "/workspace" {
			found = true
			evalSource, _ = filepath.EvalSymlinks(v.Source)
			break
		}
	}
	if !found {
		t.Errorf("expected volume mount for /workspace")
	}
	if evalSource != evalCustomWorkspace {
		t.Errorf("expected volume source %q, got %q", evalCustomWorkspace, evalSource)
	}

	// 2. Test relative path for --workspace
	relativeWorkspace := "some-subdir"

	os.MkdirAll(filepath.Join(tmpDir, relativeWorkspace), 0755)
	absRelativeWorkspace, _ := filepath.Abs(filepath.Join(tmpDir, relativeWorkspace))
	evalAbsRelativeWorkspace, _ := filepath.EvalSymlinks(absRelativeWorkspace)

	_, _, cfg, err = ProvisionAgent(context.Background(), "rel-agent", "claude", "", "", projectScionDir, "", "", "", relativeWorkspace)
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}
	found = false
	for _, v := range cfg.Volumes {
		if v.Target == "/workspace" {
			found = true
			evalSource, _ = filepath.EvalSymlinks(v.Source)
			break
		}
	}
	if !found {
		t.Errorf("expected volume mount for /workspace")
	}
	if evalSource != evalAbsRelativeWorkspace {
		t.Errorf("expected volume source %q, got %q", evalAbsRelativeWorkspace, evalSource)
	}

	// 3. Test --workspace succeeds in git repo
	gitDir := filepath.Join(tmpDir, "git-project")
	os.MkdirAll(filepath.Join(gitDir, ".git"), 0755)
	gitScionDir := filepath.Join(gitDir, ".scion")
	os.MkdirAll(gitScionDir, 0755)
	os.WriteFile(filepath.Join(gitDir, ".gitignore"), []byte("agents/"), 0644)

	var ws string
	_, ws, cfg, err = ProvisionAgent(context.Background(), "git-agent", "claude", "", "", gitScionDir, "", "", "", customWorkspace)
	if err != nil {
		t.Fatalf("expected no error when using --workspace in a git repository, got: %v", err)
	}
	if ws != "" {
		t.Errorf("expected empty workspace path (managed) for --workspace agent, got %q", ws)
	}
	found = false
	for _, v := range cfg.Volumes {
		if v.Target == "/workspace" {
			found = true
			evalSource, _ = filepath.EvalSymlinks(v.Source)
			break
		}
	}
	if !found {
		t.Errorf("expected volume mount for /workspace")
	}
	if evalSource != evalCustomWorkspace {
		t.Errorf("expected volume source %q, got %q", evalCustomWorkspace, evalSource)
	}
}

func TestProvisionAgentYAMLTemplate(t *testing.T) {
	tmpDir := t.TempDir()

	// Move to tmpDir to avoid being inside the project's git repo
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Mock HOME for global settings and templates
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)

	// Create a harness-config for claude
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	// Create an agnostic template with YAML config
	tplDir := filepath.Join(globalTemplatesDir, "yaml-test-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfigYAML := `default_harness_config: claude
env:
  TPL_VAR: tpl-val
  GOOGLE_CLOUD_PROJECT: my-project
auth_selectedType: vertex-ai
`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte(tplConfigYAML), 0644)

	// Project settings (minimal)
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)
	os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/"), 0644)

	// Provision agent
	agentName := "yaml-agent"
	_, _, cfg, err := ProvisionAgent(context.Background(), agentName, "yaml-test-tpl", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify harness resolved from harness-config
	if cfg.Harness != "claude" {
		t.Errorf("expected harness 'claude', got %q", cfg.Harness)
	}
	if cfg.Env["TPL_VAR"] != "tpl-val" {
		t.Errorf("expected env[TPL_VAR] = 'tpl-val', got %q", cfg.Env["TPL_VAR"])
	}
	if cfg.Env["GOOGLE_CLOUD_PROJECT"] != "my-project" {
		t.Errorf("expected env[GOOGLE_CLOUD_PROJECT] = 'my-project', got %q", cfg.Env["GOOGLE_CLOUD_PROJECT"])
	}
	if cfg.AuthSelectedType != "vertex-ai" {
		t.Errorf("expected auth_selectedType = 'vertex-ai', got %q", cfg.AuthSelectedType)
	}

	// Verify it was persisted to scion-agent.json
	agentScionJSON := filepath.Join(projectScionDir, "agents", agentName, "scion-agent.json")
	data, err := os.ReadFile(agentScionJSON)
	if err != nil {
		t.Fatal(err)
	}
	var persistedCfg struct {
		Harness string            `json:"harness"`
		Env     map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &persistedCfg); err != nil {
		t.Fatal(err)
	}
	if persistedCfg.Harness != "claude" {
		t.Errorf("persisted: expected harness 'claude', got %q", persistedCfg.Harness)
	}
	if persistedCfg.Env["TPL_VAR"] != "tpl-val" {
		t.Errorf("persisted: expected env[TPL_VAR] = 'tpl-val', got %q", persistedCfg.Env["TPL_VAR"])
	}
}

func TestProvisionAgentUsesProjectTemplate(t *testing.T) {
	tmpDir := t.TempDir()

	// Move to tmpDir — this is NOT the project's directory,
	// simulating a broker process whose CWD doesn't contain .scion.
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Mock HOME
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Create global harness-configs
	globalScionDir := filepath.Join(tmpDir, ".scion")
	seedTestHarnessConfig(t, globalScionDir, "grove-harness", "grove-harness")

	// Create a global agnostic template
	globalTplDir := filepath.Join(globalScionDir, "templates", "my-tpl")
	os.MkdirAll(globalTplDir, 0755)
	os.WriteFile(filepath.Join(globalTplDir, "scion-agent.json"), []byte(`{
		"default_harness_config": "grove-harness",
		"env": {"SOURCE": "global"}
	}`), 0644)

	// Create a project with its own version of the same template
	projectDir := filepath.Join(tmpDir, "project")
	projectPath := filepath.Join(projectDir, ".scion")
	projectTplDir := filepath.Join(projectPath, "templates", "my-tpl")
	os.MkdirAll(projectTplDir, 0755)
	os.WriteFile(filepath.Join(projectTplDir, "scion-agent.json"), []byte(`{
		"default_harness_config": "grove-harness",
		"env": {"SOURCE": "project"}
	}`), 0644)

	// Provision agent using projectPath — the project template should be used
	// even though CWD has no .scion directory.
	agentName := "project-tpl-agent"
	_, _, cfg, err := ProvisionAgent(context.Background(), agentName, "my-tpl", "", "", projectPath, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	if cfg.Harness != "grove-harness" {
		t.Errorf("expected harness 'grove-harness' (from harness-config), got %q", cfg.Harness)
	}
	if cfg.Env["SOURCE"] != "project" {
		t.Errorf("expected env[SOURCE] = 'project', got %q", cfg.Env["SOURCE"])
	}
}

func TestProvisionAgentInvalidYAMLTemplate(t *testing.T) {
	tmpDir := t.TempDir()

	// Move to tmpDir to avoid being inside the project's git repo
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Mock HOME for global settings and templates
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)

	// Create a template with invalid YAML config (commas in map entries)
	tplDir := filepath.Join(globalTemplatesDir, "invalid-yaml-tpl")
	os.MkdirAll(tplDir, 0755)
	invalidYAML := `default_harness_config: claude
env:
  "KEY1": "value1",
  "KEY2": "value2"
`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte(invalidYAML), 0644)

	// Project settings
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)
	os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/"), 0644)

	// Provision agent - should fail with an error
	agentName := "invalid-yaml-agent"
	_, _, _, err := ProvisionAgent(context.Background(), agentName, "invalid-yaml-tpl", "", "", projectScionDir, "", "", "", "")
	if err == nil {
		t.Fatal("expected error for invalid YAML template, got nil")
	}

	// Verify the error message contains useful information
	if !strings.Contains(err.Error(), "failed to load config from template") {
		t.Errorf("expected error to mention 'failed to load config from template', got: %v", err)
	}
}

func TestProvisionAgent_WritesServicesFile(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)

	// Create a harness-config for claude
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	t.Run("services written when defined", func(t *testing.T) {
		// Create an agnostic template with services defined in YAML
		tplDir := filepath.Join(globalTemplatesDir, "svc-tpl")
		os.MkdirAll(tplDir, 0755)
		tplConfigYAML := `default_harness_config: claude
services:
  - name: xvfb
    command: ["Xvfb", ":99"]
    restart: always
    env:
      DISPLAY: ":99"
  - name: chrome-mcp
    command: ["npx", "chrome-mcp"]
    restart: on-failure
`
		os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte(tplConfigYAML), 0644)

		projectDir := filepath.Join(tmpDir, "project-svc")
		projectScionDir := filepath.Join(projectDir, ".scion")
		os.MkdirAll(projectScionDir, 0755)

		agentName := "svc-agent"
		agentHome, _, _, err := ProvisionAgent(context.Background(), agentName, "svc-tpl", "", "", projectScionDir, "", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent failed: %v", err)
		}

		servicesFile := filepath.Join(agentHome, ".scion", "scion-services.yaml")
		data, err := os.ReadFile(servicesFile)
		if err != nil {
			t.Fatalf("expected scion-services.yaml to exist, got error: %v", err)
		}

		content := string(data)
		if !strings.Contains(content, "xvfb") {
			t.Errorf("scion-services.yaml should contain 'xvfb', got: %s", content)
		}
		if !strings.Contains(content, "chrome-mcp") {
			t.Errorf("scion-services.yaml should contain 'chrome-mcp', got: %s", content)
		}
	})

	t.Run("no services file when none defined", func(t *testing.T) {
		tplDir := filepath.Join(globalTemplatesDir, "no-svc-tpl")
		os.MkdirAll(tplDir, 0755)
		tplConfig := `{"default_harness_config": "claude"}`
		os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

		projectDir := filepath.Join(tmpDir, "project-nosvc")
		projectScionDir := filepath.Join(projectDir, ".scion")
		os.MkdirAll(projectScionDir, 0755)

		agentName := "no-svc-agent"
		agentHome, _, _, err := ProvisionAgent(context.Background(), agentName, "no-svc-tpl", "", "", projectScionDir, "", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent failed: %v", err)
		}

		servicesFile := filepath.Join(agentHome, ".scion", "scion-services.yaml")
		if _, err := os.Stat(servicesFile); !os.IsNotExist(err) {
			t.Errorf("expected scion-services.yaml to NOT exist when no services defined")
		}
	})
}

func TestProvisionAgent_CopiesSkillsDir(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)

	// Create a harness-config for claude
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	// Create a template with a skills/ directory containing a skill
	tplDir := filepath.Join(globalTemplatesDir, "skills-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{"default_harness_config": "claude"}`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	// Create skills in the template
	skillDir := filepath.Join(tplDir, "skills", "my-skill")
	os.MkdirAll(skillDir, 0755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# My Skill\nDoes things."), 0644)

	// Project settings
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	agentName := "skills-agent"
	agentHome, _, _, err := ProvisionAgent(context.Background(), agentName, "skills-tpl", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Claude harness should place skills at .claude/skills/
	skillMdPath := filepath.Join(agentHome, ".claude", "skills", "my-skill", "SKILL.md")
	data, err := os.ReadFile(skillMdPath)
	if err != nil {
		t.Fatalf("expected skill file at %s, got error: %v", skillMdPath, err)
	}
	if !strings.Contains(string(data), "My Skill") {
		t.Errorf("expected skill content to contain 'My Skill', got: %s", string(data))
	}
}

func TestProvisionAgent_SkillsAreTemplateOnly(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)

	// Create a harness-config for claude with its own skills (should be ignored)
	hcDir := filepath.Join(globalScionDir, "harness-configs", "claude")
	os.MkdirAll(hcDir, 0755)
	configYAML := "harness: claude\nimage: test-image:latest\nskills_dir: .claude/skills\ninstructions_file: .claude/CLAUDE.md\n"
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(configYAML), 0644)

	hcSkillDir := filepath.Join(hcDir, "skills", "base-skill")
	os.MkdirAll(hcSkillDir, 0755)
	os.WriteFile(filepath.Join(hcSkillDir, "SKILL.md"), []byte("# Base Skill"), 0644)

	// Create a template with a different skill
	tplDir := filepath.Join(globalTemplatesDir, "overlay-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{"default_harness_config": "claude"}`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	tplSkillDir := filepath.Join(tplDir, "skills", "tpl-skill")
	os.MkdirAll(tplSkillDir, 0755)
	os.WriteFile(filepath.Join(tplSkillDir, "SKILL.md"), []byte("# Template Skill"), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	agentName := "overlay-agent"
	agentHome, _, _, err := ProvisionAgent(context.Background(), agentName, "overlay-tpl", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Harness-config skills should NOT be copied (skills are template-only)
	baseSkillPath := filepath.Join(agentHome, ".claude", "skills", "base-skill", "SKILL.md")
	if _, err := os.Stat(baseSkillPath); err == nil {
		t.Errorf("harness-config skill should not be copied, but found at %s", baseSkillPath)
	}

	// Template skills should still be copied
	tplSkillPath := filepath.Join(agentHome, ".claude", "skills", "tpl-skill", "SKILL.md")
	if _, err := os.Stat(tplSkillPath); err != nil {
		t.Errorf("expected template skill at %s, got error: %v", tplSkillPath, err)
	}
}

// TestProvisionAgentGitClone_ClearsStaleWorktreeWorkspace verifies that when
// git clone mode is active, a workspace directory containing a stale worktree
// (.git file) from a previous local-mode run is cleared so sciontool can
// perform a fresh clone.
func TestProvisionAgentGitClone_ClearsStaleWorktreeWorkspace(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")
	tplDir := filepath.Join(globalScionDir, "templates", "claude")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"claude"}`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Pre-populate the workspace with stale worktree content: a .git FILE
	// (not directory) plus some code files — simulating a previous local run.
	agentsDir := filepath.Join(projectDir, ".scion", "agents")
	staleWorkspace := filepath.Join(agentsDir, "clone-agent", "workspace")
	os.MkdirAll(staleWorkspace, 0755)
	os.WriteFile(filepath.Join(staleWorkspace, ".git"), []byte("gitdir: ../../../.git/worktrees/clone-agent\n"), 0644)
	os.WriteFile(filepath.Join(staleWorkspace, "main.go"), []byte("package main\n"), 0644)

	// Provision in git clone mode.
	gitClone := &api.GitCloneConfig{
		URL:    "https://github.com/example/repo.git",
		Branch: "main",
		Depth:  1,
	}
	ctx := api.ContextWithGitClone(context.Background(), gitClone)

	_, wsPath, _, err := ProvisionAgent(ctx, "clone-agent", "claude", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// The workspace should now exist as an empty directory (stale content removed).
	if wsPath == "" {
		t.Fatal("expected non-empty workspace path for git clone mode")
	}
	entries, err := os.ReadDir(wsPath)
	if err != nil {
		t.Fatalf("workspace dir should exist: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected empty workspace after clearing stale worktree, got: %v", names)
	}
}

// TestProvisionAgentGitClone_PreservesExistingClone verifies that when git
// clone mode is active and the workspace already has a real git clone (.git
// directory), the content is preserved for the stop/restart case.
func TestProvisionAgentGitClone_PreservesExistingClone(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")
	tplDir := filepath.Join(globalScionDir, "templates", "claude")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"claude"}`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Pre-populate the workspace with a real git clone: .git as a DIRECTORY.
	agentsDir := filepath.Join(projectDir, ".scion", "agents")
	existingClone := filepath.Join(agentsDir, "restart-agent", "workspace")
	os.MkdirAll(existingClone, 0755)
	os.MkdirAll(filepath.Join(existingClone, ".git"), 0755) // real clone marker
	os.WriteFile(filepath.Join(existingClone, "main.go"), []byte("package main\n"), 0644)

	gitClone := &api.GitCloneConfig{
		URL:    "https://github.com/example/repo.git",
		Branch: "main",
		Depth:  1,
	}
	ctx := api.ContextWithGitClone(context.Background(), gitClone)

	_, wsPath, _, err := ProvisionAgent(ctx, "restart-agent", "claude", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// The workspace should still have the previous clone content.
	if _, err := os.Stat(filepath.Join(wsPath, "main.go")); err != nil {
		t.Errorf("expected main.go to be preserved in existing clone workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wsPath, ".git")); err != nil {
		t.Errorf("expected .git directory to be preserved: %v", err)
	}
}

// TestGetAgentGitClone_ClearsExistingWorkspace verifies that when GetAgent
// finds an existing agent directory (with config file) and git clone mode is
// active, the workspace is cleared so sciontool can perform a fresh clone.
// This covers the scenario where a hub-deleted agent's local directory remains
// and a new agent with the same name is created via hub dispatch.
func TestGetAgentGitClone_ClearsExistingWorkspace(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")
	tplDir := filepath.Join(globalScionDir, "templates", "claude")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"claude"}`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Create a fully provisioned agent directory with config file and
	// a populated workspace — simulating a leftover from a previous agent.
	agentDir := filepath.Join(projectScionDir, "agents", "reused-agent")
	agentWorkspace := filepath.Join(agentDir, "workspace")
	agentHome := filepath.Join(agentDir, "home")
	os.MkdirAll(agentWorkspace, 0755)
	os.MkdirAll(agentHome, 0755)
	// Write a config file so GetAgent treats this as an existing agent.
	os.WriteFile(filepath.Join(agentDir, "scion-agent.json"),
		[]byte(`{"harness":"claude","default_harness_config":"claude"}`), 0644)
	// Populate workspace with stale clone content.
	os.WriteFile(filepath.Join(agentWorkspace, ".git"),
		[]byte("gitdir: ../../../.git/worktrees/reused-agent\n"), 0644)
	os.WriteFile(filepath.Join(agentWorkspace, "main.go"),
		[]byte("package main\n"), 0644)

	gitClone := &api.GitCloneConfig{
		URL:    "https://github.com/example/repo.git",
		Branch: "main",
		Depth:  1,
	}
	ctx := api.ContextWithGitClone(context.Background(), gitClone)

	_, _, wsPath, _, err := GetAgent(ctx, "reused-agent", "claude", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent failed: %v", err)
	}

	// The workspace should now be empty — ready for sciontool to clone into.
	entries, err := os.ReadDir(wsPath)
	if err != nil {
		t.Fatalf("workspace dir should exist: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected empty workspace after clearing stale content, got: %v", names)
	}
}

// TestProvisionAgent_SharedWorkspaceRelocatesAgentState verifies that when
// SharedWorkspace context is set, the agent's prompt.md and scion-agent.json
// land at the external project-configs path rather than inside the project tree.
// This is the structural fix from .design/hub-shared-workspace-isolation.md
// — sibling agents must not see each other's state via /workspace.
func TestProvisionAgent_SharedWorkspaceRelocatesAgentState(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")
	tplDir := filepath.Join(globalScionDir, "templates", "claude")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"claude"}`), 0644)

	// Project dir with .scion as a directory plus project-id (split storage).
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)
	if err := config.WriteProjectID(projectScionDir, "550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Fatalf("WriteProjectID failed: %v", err)
	}

	sharedWorkspace := filepath.Join(tmpDir, "shared-ws")
	os.MkdirAll(sharedWorkspace, 0755)

	ctx := api.ContextWithSharedWorkspace(context.Background())

	rt := &runtime.MockRuntime{}
	mgr := NewManager(rt)
	opts := api.StartOptions{
		Name:            "shared-agent",
		Task:            "do the thing",
		Template:        "claude",
		ProjectPath:     projectScionDir,
		Workspace:       sharedWorkspace,
		SharedWorkspace: true,
	}
	if _, err := mgr.Provision(ctx, opts); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	// External per-agent dir must contain prompt.md and scion-agent.json.
	extAgentDir := filepath.Join(tmpDir, ".scion", "project-configs", "project__550e8400", ".scion", "agents", "shared-agent")
	if _, err := os.Stat(filepath.Join(extAgentDir, "prompt.md")); err != nil {
		t.Errorf("expected prompt.md at external path %s: %v", extAgentDir, err)
	}
	if _, err := os.Stat(filepath.Join(extAgentDir, "scion-agent.json")); err != nil {
		t.Errorf("expected scion-agent.json at external path %s: %v", extAgentDir, err)
	}
	taskBytes, err := os.ReadFile(filepath.Join(extAgentDir, "prompt.md"))
	if err == nil && string(taskBytes) != "do the thing" {
		t.Errorf("prompt.md content = %q, want %q", string(taskBytes), "do the thing")
	}

	// In-project agent dir must NOT exist for shared-workspace agents — that
	// is the whole point of the isolation. (Empty-but-present would also
	// leak the agent name to siblings.)
	inProjectAgentDir := filepath.Join(projectScionDir, "agents", "shared-agent")
	if _, err := os.Stat(inProjectAgentDir); err == nil {
		t.Errorf("expected no in-project agent dir at %s, but it exists", inProjectAgentDir)
	}
}

// TestProvisionAgent_SharedWorkspaceMigratesLegacyState verifies that an
// agent provisioned under the old layout (prompt.md / scion-agent.json
// in-project) gets its state moved to the external path on next provision.
func TestProvisionAgent_SharedWorkspaceMigratesLegacyState(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")
	tplDir := filepath.Join(globalScionDir, "templates", "claude")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"claude"}`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)
	if err := config.WriteProjectID(projectScionDir, "550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Fatalf("WriteProjectID failed: %v", err)
	}

	// Seed legacy in-project state from a pre-isolation provisioning.
	legacyDir := filepath.Join(projectScionDir, "agents", "legacy-agent")
	if err := os.MkdirAll(legacyDir, 0755); err != nil {
		t.Fatalf("mkdir legacyDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "prompt.md"), []byte("old task"), 0644); err != nil {
		t.Fatalf("write legacy prompt.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "scion-agent.json"), []byte(`{"harness":"claude"}`), 0644); err != nil {
		t.Fatalf("write legacy scion-agent.json: %v", err)
	}

	sharedWorkspace := filepath.Join(tmpDir, "shared-ws")
	os.MkdirAll(sharedWorkspace, 0755)

	ctx := api.ContextWithSharedWorkspace(context.Background())
	rt := &runtime.MockRuntime{}
	mgr := NewManager(rt)
	opts := api.StartOptions{
		Name:            "legacy-agent",
		Template:        "claude",
		ProjectPath:     projectScionDir,
		Workspace:       sharedWorkspace,
		SharedWorkspace: true,
	}
	if _, err := mgr.Provision(ctx, opts); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	// Legacy file must have been moved out — no prompt.md in the project tree.
	if _, err := os.Stat(filepath.Join(legacyDir, "prompt.md")); err == nil {
		t.Errorf("legacy in-project prompt.md still exists after migration")
	}
	if _, err := os.Stat(filepath.Join(legacyDir, "scion-agent.json")); err == nil {
		t.Errorf("legacy in-project scion-agent.json still exists after migration")
	}

	// External path must contain the migrated content.
	extAgentDir := filepath.Join(tmpDir, ".scion", "project-configs", "project__550e8400", ".scion", "agents", "legacy-agent")
	data, err := os.ReadFile(filepath.Join(extAgentDir, "prompt.md"))
	if err != nil {
		t.Fatalf("expected prompt.md at external path: %v", err)
	}
	if string(data) != "old task" {
		t.Errorf("migrated prompt.md content = %q, want %q", string(data), "old task")
	}
}

// TestProvisionAgent_SharedWorkspaceCredentialHelper verifies that when
// SharedWorkspace context is set, ProvisionAgent writes a git credential
// helper to the agent's home .gitconfig.
func TestProvisionAgent_SharedWorkspaceCredentialHelper(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")
	tplDir := filepath.Join(globalScionDir, "templates", "claude")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"claude"}`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Create a shared workspace directory (simulates a pre-cloned git repo)
	sharedWorkspace := filepath.Join(tmpDir, "shared-ws")
	os.MkdirAll(sharedWorkspace, 0755)

	// Set SharedWorkspace context
	ctx := api.ContextWithSharedWorkspace(context.Background())

	home, _, _, err := ProvisionAgent(ctx, "shared-agent", "claude", "", "", projectScionDir, "", "", "", sharedWorkspace)
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify .gitconfig contains the credential helper
	gitconfigPath := filepath.Join(home, ".gitconfig")
	data, err := os.ReadFile(gitconfigPath)
	if err != nil {
		t.Fatalf("failed to read .gitconfig: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "[credential]") {
		t.Errorf("expected [credential] section in .gitconfig, got:\n%s", content)
	}
	if !strings.Contains(content, "GITHUB_TOKEN") {
		t.Errorf("expected GITHUB_TOKEN reference in credential helper, got:\n%s", content)
	}
	if !strings.Contains(content, "username=oauth2") {
		t.Errorf("expected username=oauth2 in credential helper, got:\n%s", content)
	}
}

// TestProvisionAgent_SharedWorkspaceNoCredentialWithoutFlag verifies that
// when SharedWorkspace context is NOT set, no credential helper is added
// to the agent's .gitconfig.
func TestProvisionAgent_SharedWorkspaceNoCredentialWithoutFlag(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")
	tplDir := filepath.Join(globalScionDir, "templates", "claude")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"claude"}`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	customWorkspace := filepath.Join(tmpDir, "custom-ws")
	os.MkdirAll(customWorkspace, 0755)

	// No SharedWorkspace context — plain workspace mount
	home, _, _, err := ProvisionAgent(context.Background(), "plain-agent", "claude", "", "", projectScionDir, "", "", "", customWorkspace)
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify .gitconfig does NOT contain credential helper
	gitconfigPath := filepath.Join(home, ".gitconfig")
	data, err := os.ReadFile(gitconfigPath)
	if err != nil {
		// .gitconfig may not exist at all if no template provides it
		return
	}

	content := string(data)
	if strings.Contains(content, "[credential]") {
		t.Errorf("expected no [credential] section in .gitconfig for non-shared workspace, got:\n%s", content)
	}
}

// TestGetAgent_RecreatesMissingWorktree verifies that when an existing agent's
// managed workspace directory has been removed (e.g., by git worktree prune),
// GetAgent recreates the worktree instead of returning an empty workspace path.
func TestGetAgent_RecreatesMissingWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "") // Clear container context for worktree ops
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Create a git repo to act as the project root
	projectDir := filepath.Join(tmpDir, "project")
	os.MkdirAll(projectDir, 0755)
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = projectDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	// Set up .scion directory structure with templates
	scionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(filepath.Join(scionDir, "templates"), 0755)

	// Set up global scion with a harness config
	globalScionDir := filepath.Join(tmpDir, ".scion")
	os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "generic", "generic")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"generic"}`), 0644)

	agentName := "ws-agent"
	agentDir := filepath.Join(scionDir, "agents", agentName)
	agentWorkspace := filepath.Join(agentDir, "workspace")
	agentHome := config.GetAgentHomePath(scionDir, agentName)
	os.MkdirAll(agentDir, 0755)
	os.MkdirAll(agentHome, 0755)

	// Create a worktree (simulating a successful first provision)
	if err := util.CreateWorktree(agentWorkspace, agentName); err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	// Write agent config (so GetAgent treats this as an existing agent)
	os.WriteFile(filepath.Join(agentDir, "scion-agent.json"),
		[]byte(`{"harness":"generic","default_harness_config":"generic"}`), 0644)

	// Write agent-info.json to home
	os.WriteFile(filepath.Join(agentHome, "agent-info.json"),
		[]byte(`{"name":"ws-agent","template":"default"}`), 0644)

	// Verify the worktree exists
	if _, err := os.Stat(agentWorkspace); err != nil {
		t.Fatalf("expected workspace to exist after creation: %v", err)
	}

	// Remove the workspace directory (simulating worktree prune or manual cleanup)
	os.RemoveAll(agentWorkspace)
	// Also prune the worktree records so git doesn't think it still exists
	cmd := exec.Command("git", "worktree", "prune")
	cmd.Dir = projectDir
	cmd.Run()

	// Verify workspace is gone
	if _, err := os.Stat(agentWorkspace); !os.IsNotExist(err) {
		t.Fatalf("expected workspace to be removed")
	}

	// Call GetAgent — it should recreate the worktree
	_, _, wsPath, _, err := GetAgent(context.Background(), agentName, "", "", "", scionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent failed: %v", err)
	}

	// The workspace should now exist again
	if wsPath == "" {
		t.Fatal("expected GetAgent to return a non-empty workspace path after recreation")
	}
	if _, err := os.Stat(wsPath); os.IsNotExist(err) {
		t.Fatalf("expected workspace directory to exist after GetAgent recreation, but it doesn't: %s", wsPath)
	}

	// Verify a .git file exists (worktree indicator)
	if _, err := os.Stat(filepath.Join(wsPath, ".git")); os.IsNotExist(err) {
		t.Error("expected .git file in recreated workspace (worktree marker)")
	}
}

// TestGetAgent_StaleDirectoryCreatesWorkspace verifies that when an agent
// directory exists without a config file (stale/incomplete provisioning),
// GetAgent removes it and re-provisions with a valid workspace worktree.
// This is a regression test: previously the worktree recreation ran before
// the stale directory check, so GetAgent would create a worktree then
// immediately delete it along with the stale agent directory.
func TestGetAgent_StaleDirectoryCreatesWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "") // Clear container context for worktree ops
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Create a git repo to act as the project root
	projectDir := filepath.Join(tmpDir, "project")
	os.MkdirAll(projectDir, 0755)
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = projectDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	// Set up .scion directory structure with templates
	scionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(filepath.Join(scionDir, "templates"), 0755)

	// Set up global scion with a harness config
	globalScionDir := filepath.Join(tmpDir, ".scion")
	os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "generic", "generic")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"generic"}`), 0644)

	// .scion/agents/ must be gitignored for provisioning to succeed
	os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte(".scion/agents/\n"), 0644)

	agentName := "stale-agent"
	agentDir := filepath.Join(scionDir, "agents", agentName)

	// Create the agent directory WITHOUT a config file (simulates a failed
	// previous provisioning that wrote the directory but not scion-agent.json).
	os.MkdirAll(agentDir, 0755)
	// Also create a workspace subdirectory to simulate partial state
	os.MkdirAll(filepath.Join(agentDir, "workspace"), 0755)

	// Call GetAgent — it should detect the stale directory, remove it,
	// and re-provision successfully with a workspace worktree.
	_, _, wsPath, cfg, err := GetAgent(context.Background(), agentName, "", "", "", scionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent failed: %v", err)
	}

	if cfg == nil {
		t.Fatal("expected non-nil config from GetAgent")
	}

	// The workspace should exist and be a valid worktree
	if wsPath == "" {
		t.Fatal("expected GetAgent to return a non-empty workspace path")
	}
	if _, err := os.Stat(wsPath); os.IsNotExist(err) {
		t.Fatalf("expected workspace directory to exist after GetAgent, but it doesn't: %s", wsPath)
	}
	if _, err := os.Stat(filepath.Join(wsPath, ".git")); os.IsNotExist(err) {
		t.Error("expected .git file in workspace (worktree marker)")
	}
}

// TestGetAgent_BrandNewAgentCreatesWorkspace verifies that provisioning a
// brand new agent (no agent directory exists at all) in a git-backed project
// creates a valid workspace worktree. This is a regression test: previously
// the worktree recreation code would fire for any missing workspace — even for
// new agents — creating the worktree prematurely, which then caused the stale
// directory check or ProvisionAgent to fail.
func TestGetAgent_BrandNewAgentCreatesWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "") // Clear container context for worktree ops
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Create a git repo to act as the project root
	projectDir := filepath.Join(tmpDir, "project")
	os.MkdirAll(projectDir, 0755)
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = projectDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	// Set up .scion directory structure with templates
	scionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(filepath.Join(scionDir, "templates"), 0755)

	// Set up global scion with a harness config
	globalScionDir := filepath.Join(tmpDir, ".scion")
	os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "generic", "generic")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"generic"}`), 0644)

	// .scion/agents/ must be gitignored for provisioning to succeed
	os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte(".scion/agents/\n"), 0644)

	agentName := "brand-new-agent"

	// No agent directory exists at all — this is a brand new agent.
	agentDir := filepath.Join(scionDir, "agents", agentName)
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected agent dir to not exist before test")
	}

	// Call GetAgent — it should provision from scratch with a workspace worktree.
	_, _, wsPath, cfg, err := GetAgent(context.Background(), agentName, "", "", "", scionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent failed: %v", err)
	}

	if cfg == nil {
		t.Fatal("expected non-nil config from GetAgent")
	}

	// The workspace should exist and be a valid worktree
	if wsPath == "" {
		t.Fatal("expected GetAgent to return a non-empty workspace path")
	}
	if _, err := os.Stat(wsPath); os.IsNotExist(err) {
		t.Fatalf("expected workspace directory to exist after GetAgent, but it doesn't: %s", wsPath)
	}
	if _, err := os.Stat(filepath.Join(wsPath, ".git")); os.IsNotExist(err) {
		t.Error("expected .git file in workspace (worktree marker)")
	}
}

// TestGetAgent_MissingWorkspaceNonGit verifies that for a non-git project,
// GetAgent returns empty workspace when the managed workspace dir doesn't exist.
func TestGetAgent_MissingWorkspaceNonGit(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Set up global scion
	globalScionDir := filepath.Join(tmpDir, ".scion")
	os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "generic", "generic")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"generic"}`), 0644)

	// Non-git project directory
	projectDir := filepath.Join(tmpDir, "project")
	scionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(scionDir, 0755)

	agentName := "nongit-agent"
	agentDir := filepath.Join(scionDir, "agents", agentName)
	agentHome := config.GetAgentHomePath(scionDir, agentName)
	os.MkdirAll(agentDir, 0755)
	os.MkdirAll(agentHome, 0755)

	// Write agent config (existing agent, no workspace dir)
	os.WriteFile(filepath.Join(agentDir, "scion-agent.json"),
		[]byte(`{"harness":"generic","default_harness_config":"generic"}`), 0644)
	os.WriteFile(filepath.Join(agentHome, "agent-info.json"),
		[]byte(`{"name":"nongit-agent","template":"default"}`), 0644)

	_, _, wsPath, _, err := GetAgent(context.Background(), agentName, "", "", "", scionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent failed: %v", err)
	}

	// For non-git projects, workspace should be empty (external mount expected)
	if wsPath != "" {
		t.Errorf("expected empty workspace for non-git project, got: %s", wsPath)
	}
}

func TestProvisionAgent_SkillsWithMockResolver(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	// Create template with skills references
	tplDir := filepath.Join(globalTemplatesDir, "skill-ref-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "claude",
		"skills": [
			{"uri": "skill://scion/core/test-skill@1.0"}
		]
	}`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Set up mock resolver via context
	skillContent := []byte("# Test Skill\nDescription here.")
	contentHash := "sha256:test-hash-placeholder"

	resolver := &mockResolver{
		resolved: []ResolvedSkill{
			{
				Name:    "test-skill",
				URI:     "skill://scion/core/test-skill@1.0",
				Version: "1.0.0",
				Hash:    "", // Skip bundle hash verification for integration test
				Files:   []ResolvedFile{},
			},
		},
	}
	// For this test, we just verify the fail-closed and success path logic
	// without downloading — the download tests are in skill_resolver_test.go
	_ = skillContent
	_ = contentHash

	ctx := ContextWithSkillResolver(context.Background(), resolver)
	agentHome, _, _, err := ProvisionAgent(ctx, "skill-ref-agent", "skill-ref-tpl", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify resolution record was written
	recordPath := filepath.Join(agentHome, ".scion", "resolved-skills.json")
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("expected resolved-skills.json at %s, got error: %v", recordPath, err)
	}
	if !strings.Contains(string(data), "test-skill") {
		t.Errorf("resolution record should contain skill name, got: %s", string(data))
	}
	if !strings.Contains(string(data), "1.0.0") {
		t.Errorf("resolution record should contain version, got: %s", string(data))
	}
}

func TestProvisionAgent_RequiredSkillsNoResolver(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	tplDir := filepath.Join(globalTemplatesDir, "required-skill-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "claude",
		"skills": [
			{"uri": "skill://scion/core/scion@^1.0"},
			{"uri": "skill://scion/core/team-creation@^1.0"}
		]
	}`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// No resolver on context → should fail for required skills
	_, _, _, err := ProvisionAgent(context.Background(), "no-resolver-agent", "required-skill-tpl", "", "", projectScionDir, "", "", "", "")
	if err == nil {
		t.Fatal("expected provisioning to fail with required skills and no resolver")
	}
	if !strings.Contains(err.Error(), "no skill resolver available") {
		t.Errorf("error should mention no resolver, got: %v", err)
	}
	if !strings.Contains(err.Error(), "skill://scion/core/scion@^1.0") {
		t.Errorf("error should list the required skill URIs, got: %v", err)
	}
}

func TestProvisionAgent_OptionalSkillsNoResolver(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	tplDir := filepath.Join(globalTemplatesDir, "optional-skill-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "claude",
		"skills": [
			{"uri": "skill://scion/core/optional-skill@latest", "optional": true}
		]
	}`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// No resolver on context → should succeed for optional-only skills
	_, _, _, err := ProvisionAgent(context.Background(), "optional-agent", "optional-skill-tpl", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("expected provisioning to succeed with optional-only skills and no resolver, got: %v", err)
	}
}

func TestProvisionAgent_SkillsYAMLParsing(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	// Test YAML skills parsing with hyphenated keys
	tplDir := filepath.Join(globalTemplatesDir, "yaml-skills-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `default_harness_config: claude
skills:
  - uri: "skill://scion/core/scion@^1.0"
  - uri: "skill://project/custom@latest"
    as: my-custom
    optional: true
`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// This should fail because there's no resolver for the required skill
	_, _, _, err := ProvisionAgent(context.Background(), "yaml-skills-agent", "yaml-skills-tpl", "", "", projectScionDir, "", "", "", "")
	if err == nil {
		t.Fatal("expected error for required skill with no resolver")
	}
	// Verify the error mentions the correct URI from YAML
	if !strings.Contains(err.Error(), "skill://scion/core/scion@^1.0") {
		t.Errorf("error should list the YAML-parsed skill URI, got: %v", err)
	}
	// The optional skill should not appear in the error
	if strings.Contains(err.Error(), "skill://project/custom@latest") {
		t.Errorf("error should not list optional skill, got: %v", err)
	}
}

func TestProvisionAgent_SkillsResolverError(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	tplDir := filepath.Join(globalTemplatesDir, "resolver-err-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "claude",
		"skills": [{"uri": "skill://scion/core/scion@^1.0"}]
	}`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Resolver that returns a per-skill error for a required skill
	resolver := &mockResolver{
		errors: []ResolveError{
			{URI: "skill://scion/core/scion@^1.0", Code: "not_found", Message: "skill not found in registry"},
		},
	}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	_, _, _, err := ProvisionAgent(ctx, "resolver-err-agent", "resolver-err-tpl", "", "", projectScionDir, "", "", "", "")
	if err == nil {
		t.Fatal("expected error for required skill resolution failure")
	}
	if !strings.Contains(err.Error(), "could not be resolved") {
		t.Errorf("error should mention resolution failure, got: %v", err)
	}
}

func TestProvisionAgent_RequiredSkillOmittedFromResolverResponse(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	tplDir := filepath.Join(globalTemplatesDir, "omitted-required-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "claude",
		"skills": [
			{"uri": "skill://scion/core/skill-a@1.0"},
			{"uri": "skill://scion/core/skill-b@1.0"}
		]
	}`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Resolver returns only skill-a, silently omitting skill-b
	resolver := &mockResolver{
		resolved: []ResolvedSkill{
			{Name: "skill-a", URI: "skill://scion/core/skill-a@1.0", Version: "1.0.0"},
		},
	}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	_, _, _, err := ProvisionAgent(ctx, "omitted-agent", "omitted-required-tpl", "", "", projectScionDir, "", "", "", "")
	if err == nil {
		t.Fatal("expected error when required skill is missing from resolver response")
	}
	if !strings.Contains(err.Error(), "missing from resolver response") {
		t.Errorf("error should mention missing from resolver response, got: %v", err)
	}
	if !strings.Contains(err.Error(), "skill-b") {
		t.Errorf("error should mention the missing skill URI, got: %v", err)
	}
}

func TestProvisionAgent_OptionalSkillOmittedFromResolverResponse(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	tplDir := filepath.Join(globalTemplatesDir, "omitted-optional-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "claude",
		"skills": [
			{"uri": "skill://scion/core/skill-a@1.0"},
			{"uri": "skill://scion/core/skill-b@1.0", "optional": true}
		]
	}`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Resolver returns only skill-a; optional skill-b is omitted entirely
	resolver := &mockResolver{
		resolved: []ResolvedSkill{
			{Name: "skill-a", URI: "skill://scion/core/skill-a@1.0", Version: "1.0.0"},
		},
	}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	_, _, _, err := ProvisionAgent(ctx, "omitted-opt-agent", "omitted-optional-tpl", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("expected provisioning to succeed when only optional skill is omitted, got: %v", err)
	}
}

func TestProvisionAgent_UnrequestedSkillFromResolver(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	tplDir := filepath.Join(globalTemplatesDir, "extra-skill-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "claude",
		"skills": [
			{"uri": "skill://scion/core/skill-a@1.0"}
		]
	}`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Resolver returns the requested skill plus an unrequested extra one
	resolver := &mockResolver{
		resolved: []ResolvedSkill{
			{Name: "skill-a", URI: "skill://scion/core/skill-a@1.0", Version: "1.0.0"},
			{Name: "evil-skill", URI: "skill://evil/injected@1.0", Version: "1.0.0"},
		},
	}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	_, _, _, err := ProvisionAgent(ctx, "extra-skill-agent", "extra-skill-tpl", "", "", projectScionDir, "", "", "", "")
	if err == nil {
		t.Fatal("expected error when resolver returns unrequested skill")
	}
	if !strings.Contains(err.Error(), "unrequested skill") {
		t.Errorf("error should mention unrequested skill, got: %v", err)
	}
	if !strings.Contains(err.Error(), "skill://evil/injected@1.0") {
		t.Errorf("error should mention the injected skill URI, got: %v", err)
	}
}

func TestProvisionAgent_DuplicateResolvedSkill(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	os.MkdirAll(globalTemplatesDir, 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")

	tplDir := filepath.Join(globalTemplatesDir, "dup-skill-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{
		"default_harness_config": "claude",
		"skills": [
			{"uri": "skill://scion/core/skill-a@1.0"}
		]
	}`
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Resolver returns the same skill twice
	resolver := &mockResolver{
		resolved: []ResolvedSkill{
			{Name: "skill-a", URI: "skill://scion/core/skill-a@1.0", Version: "1.0.0"},
			{Name: "skill-a", URI: "skill://scion/core/skill-a@1.0", Version: "1.0.0"},
		},
	}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	_, _, _, err := ProvisionAgent(ctx, "dup-skill-agent", "dup-skill-tpl", "", "", projectScionDir, "", "", "", "")
	if err == nil {
		t.Fatal("expected error when resolver returns duplicate skill")
	}
	if !strings.Contains(err.Error(), "duplicate resolved skill") {
		t.Errorf("error should mention duplicate, got: %v", err)
	}
}

func TestParseSkillFrontmatter(t *testing.T) {
	t.Run("valid frontmatter", func(t *testing.T) {
		data := []byte("---\nname: test-skill\ndescription: A test skill\ninject_when: git_workspace\n---\n\n# Content here\n")
		fm := parseSkillFrontmatter(data)
		if fm.Name != "test-skill" {
			t.Errorf("Name=%q, want %q", fm.Name, "test-skill")
		}
		if fm.Description != "A test skill" {
			t.Errorf("Description=%q, want %q", fm.Description, "A test skill")
		}
		if fm.InjectWhen != "git_workspace" {
			t.Errorf("InjectWhen=%q, want %q", fm.InjectWhen, "git_workspace")
		}
	})

	t.Run("no frontmatter", func(t *testing.T) {
		data := []byte("# Just a markdown file\nNo frontmatter here\n")
		fm := parseSkillFrontmatter(data)
		if fm.Name != "" || fm.InjectWhen != "" {
			t.Errorf("expected zero-value frontmatter, got %+v", fm)
		}
	})

	t.Run("no inject_when field", func(t *testing.T) {
		data := []byte("---\nname: unconditional\ndescription: Always inject\n---\n\n# Content\n")
		fm := parseSkillFrontmatter(data)
		if fm.Name != "unconditional" {
			t.Errorf("Name=%q, want %q", fm.Name, "unconditional")
		}
		if fm.InjectWhen != "" {
			t.Errorf("InjectWhen=%q, want empty", fm.InjectWhen)
		}
	})

	t.Run("unclosed frontmatter", func(t *testing.T) {
		data := []byte("---\nname: broken\n# No closing delimiter\n")
		fm := parseSkillFrontmatter(data)
		if fm.Name != "" {
			t.Errorf("expected zero-value for unclosed frontmatter, got %+v", fm)
		}
	})

	t.Run("malformed YAML in frontmatter", func(t *testing.T) {
		data := []byte("---\nname: bad-skill\ninject_when:\t\tbadly indented\n  - not: valid\n---\n\n# Content\n")
		fm := parseSkillFrontmatter(data)
		// Malformed YAML returns zero-value (skill treated as unconditional)
		if fm.InjectWhen != "" {
			t.Errorf("expected empty InjectWhen for malformed YAML, got %q", fm.InjectWhen)
		}
	})
}

func TestShouldInjectSkill(t *testing.T) {
	tests := []struct {
		name       string
		injectWhen string
		ctx        workspaceSkillsInjectionContext
		want       bool
	}{
		{"unconditional always injects", "", workspaceSkillsInjectionContext{}, true},
		{"git_workspace with git", "git_workspace", workspaceSkillsInjectionContext{IsGit: true}, true},
		{"git_workspace without git", "git_workspace", workspaceSkillsInjectionContext{IsGit: false}, false},
		{"hub_enabled with hub", "hub_enabled", workspaceSkillsInjectionContext{HubEnabled: true}, true},
		{"hub_enabled without hub", "hub_enabled", workspaceSkillsInjectionContext{HubEnabled: false}, false},
		{"unknown condition skips", "unknown_condition", workspaceSkillsInjectionContext{IsGit: true, HubEnabled: true}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm := skillFrontmatter{Name: "test", InjectWhen: tt.injectWhen}
			got := shouldInjectSkill(fm, tt.ctx)
			if got != tt.want {
				t.Errorf("shouldInjectSkill(inject_when=%q)=%v, want %v", tt.injectWhen, got, tt.want)
			}
		})
	}
}

// createTestSkill creates a skill directory with a SKILL.md file.
func createTestSkill(t *testing.T, baseDir, name, content string) {
	t.Helper()
	skillDir := filepath.Join(baseDir, name)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("failed to create skill dir %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatalf("failed to create test skill %s: %v", name, err)
	}
}

// setupWorkspaceSkillsTest creates an isolated project layout for workspace
// skills injection testing. Returns projectDir, wsSkillsDir, agentHome, skillsDir.
func setupWorkspaceSkillsTest(t *testing.T) (string, string, string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}
	wsSkillsDir := filepath.Join(tmpDir, "skills")
	if err := os.MkdirAll(wsSkillsDir, 0755); err != nil {
		t.Fatalf("failed to create skills dir: %v", err)
	}
	agentHome := filepath.Join(tmpDir, "agent-home")
	skillsDir := ".claude/skills"
	if err := os.MkdirAll(filepath.Join(agentHome, skillsDir), 0755); err != nil {
		t.Fatalf("failed to create agent home skills dir: %v", err)
	}
	return projectDir, wsSkillsDir, agentHome, skillsDir
}

func TestInjectWorkspaceSkills_HarnessWithSkillsDir(t *testing.T) {
	t.Run("unconditional skills are copied", func(t *testing.T) {
		projectDir, wsSkillsDir, agentHome, skillsDir := setupWorkspaceSkillsTest(t)
		createTestSkill(t, wsSkillsDir, "always-skill", "---\nname: always-skill\ndescription: Always inject\n---\n\n# Always\n")

		injCtx := workspaceSkillsInjectionContext{IsGit: false, HubEnabled: false}
		_, err := injectWorkspaceSkills(projectDir, agentHome, skillsDir, injCtx, nil)
		if err != nil {
			t.Fatalf("injectWorkspaceSkills failed: %v", err)
		}

		dest := filepath.Join(agentHome, skillsDir, "always-skill", "SKILL.md")
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			t.Errorf("expected skill to be copied to %s", dest)
		}
	})

	t.Run("git_workspace skill injected when isGit=true", func(t *testing.T) {
		projectDir, wsSkillsDir, agentHome, skillsDir := setupWorkspaceSkillsTest(t)
		createTestSkill(t, wsSkillsDir, "git-skill", "---\nname: git-skill\ninject_when: git_workspace\n---\n\n# Git\n")

		injCtx := workspaceSkillsInjectionContext{IsGit: true}
		_, err := injectWorkspaceSkills(projectDir, agentHome, skillsDir, injCtx, nil)
		if err != nil {
			t.Fatalf("injectWorkspaceSkills failed: %v", err)
		}

		dest := filepath.Join(agentHome, skillsDir, "git-skill", "SKILL.md")
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			t.Errorf("expected git-skill to be copied when isGit=true")
		}
	})

	t.Run("git_workspace skill skipped when isGit=false", func(t *testing.T) {
		projectDir, wsSkillsDir, agentHome, skillsDir := setupWorkspaceSkillsTest(t)
		createTestSkill(t, wsSkillsDir, "git-skill", "---\nname: git-skill\ninject_when: git_workspace\n---\n\n# Git\n")

		injCtx := workspaceSkillsInjectionContext{IsGit: false}
		_, err := injectWorkspaceSkills(projectDir, agentHome, skillsDir, injCtx, nil)
		if err != nil {
			t.Fatalf("injectWorkspaceSkills failed: %v", err)
		}

		dest := filepath.Join(agentHome, skillsDir, "git-skill")
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Errorf("expected git-skill to NOT be copied when isGit=false")
		}
	})

	t.Run("template skill takes precedence", func(t *testing.T) {
		projectDir, wsSkillsDir, agentHome, skillsDir := setupWorkspaceSkillsTest(t)

		templateContent := "template version"
		tplSkillDir := filepath.Join(agentHome, skillsDir, "conflict-skill")
		if err := os.MkdirAll(tplSkillDir, 0755); err != nil {
			t.Fatalf("failed to create template skill dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(tplSkillDir, "SKILL.md"), []byte(templateContent), 0644); err != nil {
			t.Fatalf("failed to write template skill: %v", err)
		}

		createTestSkill(t, wsSkillsDir, "conflict-skill", "---\nname: conflict-skill\n---\n\nworkspace version")

		injCtx := workspaceSkillsInjectionContext{}
		_, err := injectWorkspaceSkills(projectDir, agentHome, skillsDir, injCtx, nil)
		if err != nil {
			t.Fatalf("injectWorkspaceSkills failed: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(tplSkillDir, "SKILL.md"))
		if err != nil {
			t.Fatalf("failed to read skill: %v", err)
		}
		if string(data) != templateContent {
			t.Errorf("template skill was overwritten: got %q, want %q", string(data), templateContent)
		}
	})

	t.Run("no workspace skills dir is graceful", func(t *testing.T) {
		noSkillsProject := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(noSkillsProject, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		agentHome := filepath.Join(t.TempDir(), "agent-home")
		if err := os.MkdirAll(agentHome, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}

		injCtx := workspaceSkillsInjectionContext{}
		_, err := injectWorkspaceSkills(noSkillsProject, agentHome, ".claude/skills", injCtx, nil)
		if err != nil {
			t.Errorf("expected graceful handling of missing skills dir, got: %v", err)
		}
	})

	t.Run("hidden directories are skipped", func(t *testing.T) {
		projectDir, wsSkillsDir, agentHome, skillsDir := setupWorkspaceSkillsTest(t)

		hiddenDir := filepath.Join(wsSkillsDir, ".hidden-skill")
		if err := os.MkdirAll(hiddenDir, 0755); err != nil {
			t.Fatalf("failed to create hidden dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(hiddenDir, "SKILL.md"), []byte("---\nname: hidden\n---\n"), 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}

		injCtx := workspaceSkillsInjectionContext{}
		_, err := injectWorkspaceSkills(projectDir, agentHome, skillsDir, injCtx, nil)
		if err != nil {
			t.Fatalf("injectWorkspaceSkills failed: %v", err)
		}

		dest := filepath.Join(agentHome, skillsDir, ".hidden-skill")
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Errorf("expected hidden directory to be skipped")
		}
	})

	t.Run("non-directory files in skills dir are skipped", func(t *testing.T) {
		projectDir, wsSkillsDir, agentHome, skillsDir := setupWorkspaceSkillsTest(t)

		readmePath := filepath.Join(wsSkillsDir, "README.md")
		if err := os.WriteFile(readmePath, []byte("# Skills readme"), 0644); err != nil {
			t.Fatalf("failed to write readme: %v", err)
		}

		injCtx := workspaceSkillsInjectionContext{}
		_, err := injectWorkspaceSkills(projectDir, agentHome, skillsDir, injCtx, nil)
		if err != nil {
			t.Fatalf("injectWorkspaceSkills failed: %v", err)
		}

		dest := filepath.Join(agentHome, skillsDir, "README.md")
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Errorf("expected non-directory file to be skipped")
		}
	})
}

func TestInjectWorkspaceSkills_FallbackComposition(t *testing.T) {
	tmpDir := t.TempDir()

	projectDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}
	wsSkillsDir := filepath.Join(tmpDir, "skills")
	if err := os.MkdirAll(wsSkillsDir, 0755); err != nil {
		t.Fatalf("failed to create skills dir: %v", err)
	}

	agentHome := filepath.Join(tmpDir, "agent-home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatalf("failed to create agent home: %v", err)
	}

	t.Run("SKILL.md content appended when no skills dir", func(t *testing.T) {
		createTestSkill(t, wsSkillsDir, "fallback-skill", "---\nname: fallback-skill\n---\n\n# Fallback content\n")

		injCtx := workspaceSkillsInjectionContext{}
		result, err := injectWorkspaceSkills(projectDir, agentHome, "", injCtx, []byte("base instructions"))
		if err != nil {
			t.Fatalf("injectWorkspaceSkills failed: %v", err)
		}

		if !strings.Contains(string(result), "# Fallback content") {
			t.Errorf("expected SKILL.md content to be appended, got: %q", string(result))
		}
		if !strings.Contains(string(result), "base instructions") {
			t.Errorf("expected base content to be preserved")
		}
	})

	t.Run("conditional skills are respected in fallback mode", func(t *testing.T) {
		createTestSkill(t, wsSkillsDir, "git-only-fallback", "---\nname: git-only-fallback\ninject_when: git_workspace\n---\n\n# Git only\n")

		injCtx := workspaceSkillsInjectionContext{IsGit: false}
		result, err := injectWorkspaceSkills(projectDir, agentHome, "", injCtx, []byte("base"))
		if err != nil {
			t.Fatalf("injectWorkspaceSkills failed: %v", err)
		}

		if strings.Contains(string(result), "# Git only") {
			t.Errorf("expected git-only skill to be skipped when isGit=false")
		}
	})

	t.Run("skill without SKILL.md is skipped in fallback", func(t *testing.T) {
		isolatedDir := t.TempDir()
		isolatedProject := filepath.Join(isolatedDir, ".scion")
		if err := os.MkdirAll(isolatedProject, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		isolatedSkills := filepath.Join(isolatedDir, "skills")
		if err := os.MkdirAll(isolatedSkills, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}

		noMDSkill := filepath.Join(isolatedSkills, "no-skillmd")
		if err := os.MkdirAll(noMDSkill, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}

		injCtx := workspaceSkillsInjectionContext{}
		result, err := injectWorkspaceSkills(isolatedProject, agentHome, "", injCtx, []byte("base"))
		if err != nil {
			t.Fatalf("injectWorkspaceSkills failed: %v", err)
		}

		if string(result) != "base" {
			t.Errorf("expected no change for skill without SKILL.md, got: %q", string(result))
		}
	})

	t.Run("hub_enabled skill injected in fallback when hub active", func(t *testing.T) {
		createTestSkill(t, wsSkillsDir, "hub-fallback", "---\nname: hub-fallback\ninject_when: hub_enabled\n---\n\n# Hub content\n")

		injCtx := workspaceSkillsInjectionContext{HubEnabled: true}
		result, err := injectWorkspaceSkills(projectDir, agentHome, "", injCtx, []byte("base"))
		if err != nil {
			t.Fatalf("injectWorkspaceSkills failed: %v", err)
		}

		if !strings.Contains(string(result), "# Hub content") {
			t.Errorf("expected hub skill content when hubEnabled=true")
		}
	})
}

func TestInjectPlatformSkills(t *testing.T) {
	t.Run("unconditional skills are injected", func(t *testing.T) {
		agentHome := t.TempDir()
		skillsDir := ".claude/commands"

		skillsFS := fstest.MapFS{
			"my-skill/SKILL.md": &fstest.MapFile{
				Data: []byte("---\nname: my-skill\ndescription: A test skill\n---\n\n# My Skill\n"),
			},
		}

		injCtx := workspaceSkillsInjectionContext{IsGit: false, HubEnabled: false}
		if err := injectPlatformSkills(skillsFS, agentHome, skillsDir, injCtx); err != nil {
			t.Fatalf("injectPlatformSkills failed: %v", err)
		}

		dest := filepath.Join(agentHome, skillsDir, "my-skill", "SKILL.md")
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			t.Errorf("expected platform skill to be copied to %s", dest)
		}
	})

	t.Run("git_workspace skill injected when isGit=true", func(t *testing.T) {
		agentHome := t.TempDir()
		skillsDir := ".claude/commands"

		skillsFS := fstest.MapFS{
			"git-skill/SKILL.md": &fstest.MapFile{
				Data: []byte("---\nname: git-skill\ninject_when: git_workspace\n---\n\n# Git\n"),
			},
		}

		injCtx := workspaceSkillsInjectionContext{IsGit: true}
		if err := injectPlatformSkills(skillsFS, agentHome, skillsDir, injCtx); err != nil {
			t.Fatalf("injectPlatformSkills failed: %v", err)
		}

		dest := filepath.Join(agentHome, skillsDir, "git-skill", "SKILL.md")
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			t.Errorf("expected git-skill to be injected when isGit=true")
		}
	})

	t.Run("git_workspace skill skipped when isGit=false", func(t *testing.T) {
		agentHome := t.TempDir()
		skillsDir := ".claude/commands"

		skillsFS := fstest.MapFS{
			"git-skill/SKILL.md": &fstest.MapFile{
				Data: []byte("---\nname: git-skill\ninject_when: git_workspace\n---\n\n# Git\n"),
			},
		}

		injCtx := workspaceSkillsInjectionContext{IsGit: false}
		if err := injectPlatformSkills(skillsFS, agentHome, skillsDir, injCtx); err != nil {
			t.Fatalf("injectPlatformSkills failed: %v", err)
		}

		dest := filepath.Join(agentHome, skillsDir, "git-skill")
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Errorf("expected git-skill to NOT be injected when isGit=false")
		}
	})

	t.Run("template skill takes precedence over platform skill", func(t *testing.T) {
		agentHome := t.TempDir()
		skillsDir := ".claude/commands"

		tplContent := "template version"
		tplSkillDir := filepath.Join(agentHome, skillsDir, "conflict-skill")
		if err := os.MkdirAll(tplSkillDir, 0755); err != nil {
			t.Fatalf("failed to create template skill dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(tplSkillDir, "SKILL.md"), []byte(tplContent), 0644); err != nil {
			t.Fatalf("failed to write template skill: %v", err)
		}

		skillsFS := fstest.MapFS{
			"conflict-skill/SKILL.md": &fstest.MapFile{
				Data: []byte("---\nname: conflict-skill\n---\n\nplatform version"),
			},
		}

		injCtx := workspaceSkillsInjectionContext{}
		if err := injectPlatformSkills(skillsFS, agentHome, skillsDir, injCtx); err != nil {
			t.Fatalf("injectPlatformSkills failed: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(tplSkillDir, "SKILL.md"))
		if err != nil {
			t.Fatalf("failed to read skill: %v", err)
		}
		if string(data) != tplContent {
			t.Errorf("template skill was overwritten: got %q, want %q", string(data), tplContent)
		}
	})

	t.Run("skill with scripts subdirectory is fully copied", func(t *testing.T) {
		agentHome := t.TempDir()
		skillsDir := ".claude/commands"

		skillsFS := fstest.MapFS{
			"scion/SKILL.md": &fstest.MapFile{
				Data: []byte("---\nname: scion\n---\n\n# Scion\n"),
			},
			"scion/scripts/start-agent.sh": &fstest.MapFile{
				Data: []byte("#!/bin/bash\necho start"),
			},
		}

		injCtx := workspaceSkillsInjectionContext{}
		if err := injectPlatformSkills(skillsFS, agentHome, skillsDir, injCtx); err != nil {
			t.Fatalf("injectPlatformSkills failed: %v", err)
		}

		dest := filepath.Join(agentHome, skillsDir, "scion", "scripts", "start-agent.sh")
		data, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("expected script to be copied, got error: %v", err)
		}
		if !strings.Contains(string(data), "echo start") {
			t.Errorf("script content mismatch: %q", string(data))
		}
	})

	t.Run("multiple skills with mixed conditions", func(t *testing.T) {
		agentHome := t.TempDir()
		skillsDir := ".claude/commands"

		skillsFS := fstest.MapFS{
			"always-skill/SKILL.md": &fstest.MapFile{
				Data: []byte("---\nname: always-skill\n---\n\n# Always\n"),
			},
			"git-only/SKILL.md": &fstest.MapFile{
				Data: []byte("---\nname: git-only\ninject_when: git_workspace\n---\n\n# Git Only\n"),
			},
		}

		injCtx := workspaceSkillsInjectionContext{IsGit: false}
		if err := injectPlatformSkills(skillsFS, agentHome, skillsDir, injCtx); err != nil {
			t.Fatalf("injectPlatformSkills failed: %v", err)
		}

		if _, err := os.Stat(filepath.Join(agentHome, skillsDir, "always-skill", "SKILL.md")); os.IsNotExist(err) {
			t.Errorf("expected unconditional skill to be injected")
		}
		if _, err := os.Stat(filepath.Join(agentHome, skillsDir, "git-only")); !os.IsNotExist(err) {
			t.Errorf("expected git-only skill to be skipped when isGit=false")
		}
	})
}
