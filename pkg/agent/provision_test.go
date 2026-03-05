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
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/runtime"
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
	_, _, _, err := ProvisionAgent(context.Background(), agentName, "default", "", "gemini", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify agent's settings.json (copied from gemini harness-config's home)
	agentSettingsPath := filepath.Join(projectScionDir, "agents", agentName, "home", ".gemini", "settings.json")
	data, err := os.ReadFile(agentSettingsPath)
	if err != nil {
		t.Fatalf("failed to read agent settings.json: %v", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to unmarshal agent settings.json: %v", err)
	}

	security, ok := settings["security"].(map[string]interface{})
	if !ok {
		t.Fatal("expected security block in settings.json")
	}
	auth, ok := security["auth"].(map[string]interface{})
	if !ok {
		t.Fatal("expected security.auth block in settings.json")
	}
	// The embed no longer hardcodes selectedType; Provision() sets it dynamically
	// from the harness-config's auth_selected_type ("api-key"), mapped to the
	// Gemini CLI internal format ("gemini-api-key").
	if auth["selectedType"] != "gemini-api-key" {
		t.Errorf("expected selectedType gemini-api-key (mapped from api-key), got %v", auth["selectedType"])
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

	t.Run("with task", func(t *testing.T) {
		opts := api.StartOptions{
			Name:      "agent-with-task",
			Task:      "implement feature X",
			Template:  "default",
			GrovePath: projectScionDir,
		}

		_, err := mgr.Provision(context.Background(), opts)
		if err != nil {
			t.Fatalf("Provision failed: %v", err)
		}

		promptFile := filepath.Join(projectScionDir, "agents", "agent-with-task", "prompt.md")
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
			GrovePath: projectScionDir,
		}

		_, err := mgr.Provision(context.Background(), opts)
		if err != nil {
			t.Fatalf("Provision failed: %v", err)
		}

		promptFile := filepath.Join(projectScionDir, "agents", "agent-no-task", "prompt.md")
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

func TestProvisionAgentUsesGroveTemplate(t *testing.T) {
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
	grovePath := filepath.Join(projectDir, ".scion")
	groveTplDir := filepath.Join(grovePath, "templates", "my-tpl")
	os.MkdirAll(groveTplDir, 0755)
	os.WriteFile(filepath.Join(groveTplDir, "scion-agent.json"), []byte(`{
		"default_harness_config": "grove-harness",
		"env": {"SOURCE": "grove"}
	}`), 0644)

	// Provision agent using grovePath — the grove template should be used
	// even though CWD has no .scion directory.
	agentName := "grove-tpl-agent"
	_, _, cfg, err := ProvisionAgent(context.Background(), agentName, "my-tpl", "", "", grovePath, "", "", "", "")
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
