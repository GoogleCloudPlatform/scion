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

	// Provision a gemini agent using the "default" agnostic template
	agentName := "gemini-agent"
	agentHome, _, _, err := ProvisionAgent(context.Background(), agentName, "default", "", "gemini", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify agent's settings.json (copied from gemini harness-config's home)
	agentSettingsPath := filepath.Join(agentHome, ".gemini", "settings.json")
	data, err := os.ReadFile(agentSettingsPath)
	if err != nil {
		t.Fatalf("failed to read agent settings.json: %v", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to unmarshal agent settings.json: %v", err)
	}

	// With no auth_selected_type in the gemini harness config, Provision()
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

	// Resolve the actual grove directory (may be external for non-git groves)
	resolvedGroveDir, _ := config.GetResolvedProjectDir(projectScionDir)

	t.Run("with task", func(t *testing.T) {
		opts := api.StartOptions{
			Name:      "agent-with-task",
			Task:      "implement feature X",
			Template:  "default",
			ProjectPath: projectScionDir,
		}

		_, err := mgr.Provision(context.Background(), opts)
		if err != nil {
			t.Fatalf("Provision failed: %v", err)
		}

		promptFile := filepath.Join(resolvedGroveDir, "agents", "agent-with-task", "prompt.md")
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
			Name:      "agent-no-task",
			Template:  "default",
			ProjectPath: projectScionDir,
		}

		_, err := mgr.Provision(context.Background(), opts)
		if err != nil {
			t.Fatalf("Provision failed: %v", err)
		}

		promptFile := filepath.Join(resolvedGroveDir, "agents", "agent-no-task", "prompt.md")
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

	// Project-local grove
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

	// Global grove
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
		t.Fatalf("ProvisionAgent failed for global grove: %v", err)
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
	seedTestHarnessConfig(t, globalScionDir, "gemini", "gemini")

	tplDir := filepath.Join(globalTemplatesDir, "gemini")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{"default_harness_config": "gemini"}`
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
	_, _, cfg, err := ProvisionAgent(context.Background(), agentName, "gemini", "", "", projectScionDir, "", "", "", customWorkspace)
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

	_, _, cfg, err = ProvisionAgent(context.Background(), "rel-agent", "gemini", "", "", projectScionDir, "", "", "", relativeWorkspace)
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
	_, ws, cfg, err = ProvisionAgent(context.Background(), "git-agent", "gemini", "", "", gitScionDir, "", "", "", customWorkspace)
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

	// Create a harness-config for gemini
	seedTestHarnessConfig(t, globalScionDir, "gemini", "gemini")

	// Create an agnostic template with YAML config
	tplDir := filepath.Join(globalTemplatesDir, "yaml-test-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfigYAML := `default_harness_config: gemini
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
	if cfg.Harness != "gemini" {
		t.Errorf("expected harness 'gemini', got %q", cfg.Harness)
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
	if persistedCfg.Harness != "gemini" {
		t.Errorf("persisted: expected harness 'gemini', got %q", persistedCfg.Harness)
	}
	if persistedCfg.Env["TPL_VAR"] != "tpl-val" {
		t.Errorf("persisted: expected env[TPL_VAR] = 'tpl-val', got %q", persistedCfg.Env["TPL_VAR"])
	}
}

func TestProvisionAgentUsesProjectTemplate(t *testing.T) {
	tmpDir := t.TempDir()

	// Move to tmpDir — this is NOT the grove's directory,
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

	// Create a grove with its own version of the same template
	projectDir := filepath.Join(tmpDir, "project")
	projectPath := filepath.Join(projectDir, ".scion")
	groveTplDir := filepath.Join(projectPath, "templates", "my-tpl")
	os.MkdirAll(groveTplDir, 0755)
	os.WriteFile(filepath.Join(groveTplDir, "scion-agent.json"), []byte(`{
		"default_harness_config": "grove-harness",
		"env": {"SOURCE": "grove"}
	}`), 0644)

	// Provision agent using projectPath — the grove template should be used
	// even though CWD has no .scion directory.
	agentName := "grove-tpl-agent"
	_, _, cfg, err := ProvisionAgent(context.Background(), agentName, "my-tpl", "", "", projectPath, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	if cfg.Harness != "grove-harness" {
		t.Errorf("expected harness 'grove-harness' (from harness-config), got %q", cfg.Harness)
	}
	if cfg.Env["SOURCE"] != "grove" {
		t.Errorf("expected env[SOURCE] = 'grove', got %q", cfg.Env["SOURCE"])
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
	invalidYAML := `default_harness_config: gemini
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

	// Create a harness-config for gemini
	seedTestHarnessConfig(t, globalScionDir, "gemini", "gemini")

	t.Run("services written when defined", func(t *testing.T) {
		// Create an agnostic template with services defined in YAML
		tplDir := filepath.Join(globalTemplatesDir, "svc-tpl")
		os.MkdirAll(tplDir, 0755)
		tplConfigYAML := `default_harness_config: gemini
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
		tplConfig := `{"default_harness_config": "gemini"}`
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

func TestAppendExtraInstructions(t *testing.T) {
	base := []byte("base instructions")
	ctx := context.Background()

	t.Run("no git no hub returns unchanged", func(t *testing.T) {
		result := appendExtraInstructions(ctx, base, false, nil)
		if string(result) != string(base) {
			t.Errorf("expected unchanged content, got %q", string(result))
		}
	})

	t.Run("nil settings returns unchanged for non-git", func(t *testing.T) {
		result := appendExtraInstructions(ctx, base, false, nil)
		if string(result) != string(base) {
			t.Errorf("expected unchanged content, got %q", string(result))
		}
	})

	t.Run("git true appends agents-git.md content", func(t *testing.T) {
		result := appendExtraInstructions(ctx, base, true, nil)
		if string(result) == string(base) {
			t.Errorf("expected content to be appended for git=true")
		}
		if !strings.Contains(string(result), string(base)) {
			t.Errorf("result should contain base content")
		}
		if !strings.Contains(string(result), "Git Workflow Protocol") {
			t.Errorf("result should contain git workflow content from agents-git.md")
		}
	})

	t.Run("hub enabled appends agents-hub.md content", func(t *testing.T) {
		enabled := true
		settings := &config.VersionedSettings{
			Hub: &config.V1HubClientConfig{
				Enabled: &enabled,
			},
		}
		result := appendExtraInstructions(ctx, base, false, settings)
		if string(result) == string(base) {
			t.Errorf("expected content to be appended for hub enabled")
		}
		if !strings.Contains(string(result), string(base)) {
			t.Errorf("result should contain base content")
		}
		if !strings.Contains(string(result), "Scion CLI Operating Instructions") {
			t.Errorf("result should contain hub instructions from agents-hub.md")
		}
	})

	t.Run("hub disabled does not append", func(t *testing.T) {
		disabled := false
		settings := &config.VersionedSettings{
			Hub: &config.V1HubClientConfig{
				Enabled: &disabled,
			},
		}
		result := appendExtraInstructions(ctx, base, false, settings)
		if string(result) != string(base) {
			t.Errorf("expected unchanged content, got %q", string(result))
		}
	})

	t.Run("broker mode appends agents-hub.md even without hub settings", func(t *testing.T) {
		brokerCtx := api.ContextWithBrokerMode(ctx)
		result := appendExtraInstructions(brokerCtx, base, false, nil)
		if string(result) == string(base) {
			t.Errorf("expected content to be appended for broker mode")
		}
		if !strings.Contains(string(result), "Scion CLI Operating Instructions") {
			t.Errorf("result should contain hub instructions from agents-hub.md in broker mode")
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

func TestProvisionAgent_SkillsDirOverlay(t *testing.T) {
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

	// Create a harness-config for gemini with its own skills
	hcDir := filepath.Join(globalScionDir, "harness-configs", "gemini")
	os.MkdirAll(hcDir, 0755)
	configYAML := "harness: gemini\nimage: test-image:latest\n"
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(configYAML), 0644)

	hcSkillDir := filepath.Join(hcDir, "skills", "base-skill")
	os.MkdirAll(hcSkillDir, 0755)
	os.WriteFile(filepath.Join(hcSkillDir, "SKILL.md"), []byte("# Base Skill"), 0644)

	// Create a template with a different skill
	tplDir := filepath.Join(globalTemplatesDir, "overlay-tpl")
	os.MkdirAll(tplDir, 0755)
	tplConfig := `{"default_harness_config": "gemini"}`
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

	// Both skills should exist (overlay/merge behavior)
	baseSkillPath := filepath.Join(agentHome, ".gemini", "skills", "base-skill", "SKILL.md")
	if _, err := os.Stat(baseSkillPath); err != nil {
		t.Errorf("expected base skill from harness-config at %s, got error: %v", baseSkillPath, err)
	}

	tplSkillPath := filepath.Join(agentHome, ".gemini", "skills", "tpl-skill", "SKILL.md")
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
	seedTestHarnessConfig(t, globalScionDir, "gemini", "gemini")
	tplDir := filepath.Join(globalScionDir, "templates", "gemini")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"gemini"}`), 0644)

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

	_, wsPath, _, err := ProvisionAgent(ctx, "clone-agent", "gemini", "", "", projectScionDir, "", "", "", "")
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
	seedTestHarnessConfig(t, globalScionDir, "gemini", "gemini")
	tplDir := filepath.Join(globalScionDir, "templates", "gemini")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"gemini"}`), 0644)

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

	_, wsPath, _, err := ProvisionAgent(ctx, "restart-agent", "gemini", "", "", projectScionDir, "", "", "", "")
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
	seedTestHarnessConfig(t, globalScionDir, "gemini", "gemini")
	tplDir := filepath.Join(globalScionDir, "templates", "gemini")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"gemini"}`), 0644)

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
		[]byte(`{"harness":"gemini","default_harness_config":"gemini"}`), 0644)
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

	_, _, wsPath, _, err := GetAgent(ctx, "reused-agent", "gemini", "", "", projectScionDir, "", "", "", "")
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
// land at the external project-configs path rather than inside the grove tree.
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
	seedTestHarnessConfig(t, globalScionDir, "gemini", "gemini")
	tplDir := filepath.Join(globalScionDir, "templates", "gemini")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"gemini"}`), 0644)

	// Project dir with .scion as a directory plus grove-id (split storage).
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
		Template:        "gemini",
		ProjectPath:       projectScionDir,
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

	// In-grove agent dir must NOT exist for shared-workspace agents — that
	// is the whole point of the isolation. (Empty-but-present would also
	// leak the agent name to siblings.)
	inGroveAgentDir := filepath.Join(projectScionDir, "agents", "shared-agent")
	if _, err := os.Stat(inGroveAgentDir); err == nil {
		t.Errorf("expected no in-grove agent dir at %s, but it exists", inGroveAgentDir)
	}
}

// TestProvisionAgent_SharedWorkspaceMigratesLegacyState verifies that an
// agent provisioned under the old layout (prompt.md / scion-agent.json
// in-grove) gets its state moved to the external path on next provision.
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
	seedTestHarnessConfig(t, globalScionDir, "gemini", "gemini")
	tplDir := filepath.Join(globalScionDir, "templates", "gemini")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"gemini"}`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)
	if err := config.WriteProjectID(projectScionDir, "550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Fatalf("WriteProjectID failed: %v", err)
	}

	// Seed legacy in-grove state from a pre-isolation provisioning.
	legacyDir := filepath.Join(projectScionDir, "agents", "legacy-agent")
	if err := os.MkdirAll(legacyDir, 0755); err != nil {
		t.Fatalf("mkdir legacyDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "prompt.md"), []byte("old task"), 0644); err != nil {
		t.Fatalf("write legacy prompt.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "scion-agent.json"), []byte(`{"harness":"gemini"}`), 0644); err != nil {
		t.Fatalf("write legacy scion-agent.json: %v", err)
	}

	sharedWorkspace := filepath.Join(tmpDir, "shared-ws")
	os.MkdirAll(sharedWorkspace, 0755)

	ctx := api.ContextWithSharedWorkspace(context.Background())
	rt := &runtime.MockRuntime{}
	mgr := NewManager(rt)
	opts := api.StartOptions{
		Name:            "legacy-agent",
		Template:        "gemini",
		ProjectPath:       projectScionDir,
		Workspace:       sharedWorkspace,
		SharedWorkspace: true,
	}
	if _, err := mgr.Provision(ctx, opts); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	// Legacy file must have been moved out — no prompt.md in the grove tree.
	if _, err := os.Stat(filepath.Join(legacyDir, "prompt.md")); err == nil {
		t.Errorf("legacy in-grove prompt.md still exists after migration")
	}
	if _, err := os.Stat(filepath.Join(legacyDir, "scion-agent.json")); err == nil {
		t.Errorf("legacy in-grove scion-agent.json still exists after migration")
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
	seedTestHarnessConfig(t, globalScionDir, "gemini", "gemini")
	tplDir := filepath.Join(globalScionDir, "templates", "gemini")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"gemini"}`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Create a shared workspace directory (simulates a pre-cloned git repo)
	sharedWorkspace := filepath.Join(tmpDir, "shared-ws")
	os.MkdirAll(sharedWorkspace, 0755)

	// Set SharedWorkspace context
	ctx := api.ContextWithSharedWorkspace(context.Background())

	home, _, _, err := ProvisionAgent(ctx, "shared-agent", "gemini", "", "", projectScionDir, "", "", "", sharedWorkspace)
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
	seedTestHarnessConfig(t, globalScionDir, "gemini", "gemini")
	tplDir := filepath.Join(globalScionDir, "templates", "gemini")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"gemini"}`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	customWorkspace := filepath.Join(tmpDir, "custom-ws")
	os.MkdirAll(customWorkspace, 0755)

	// No SharedWorkspace context — plain workspace mount
	home, _, _, err := ProvisionAgent(context.Background(), "plain-agent", "gemini", "", "", projectScionDir, "", "", "", customWorkspace)
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
