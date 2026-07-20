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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// setupCompositionTest creates a standard test environment with HOME, global .scion dir,
// a project directory, and returns cleanup info.
func setupCompositionTest(t *testing.T) (tmpDir, globalScionDir, projectScionDir string) {
	t.Helper()
	tmpDir = t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	t.Setenv("HOME", tmpDir)

	globalScionDir = filepath.Join(tmpDir, ".scion")
	_ = os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)

	// Seed the default template so template chain inheritance works
	defaultTplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := config.SeedAgnosticTemplate(defaultTplDir, false); err != nil {
		t.Fatalf("failed to seed default template: %v", err)
	}

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir = filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	return tmpDir, globalScionDir, projectScionDir
}

func TestComposition_HarnessConfigBaseLayer(t *testing.T) {
	_, globalScionDir, projectScionDir := setupCompositionTest(t)

	// Create harness-config with home files
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-hc")
	hcHome := filepath.Join(hcDir, "home")
	_ = os.MkdirAll(filepath.Join(hcHome, ".config"), 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\n"), 0644)
	_ = os.WriteFile(filepath.Join(hcHome, "base-file.txt"), []byte("from-harness-config"), 0644)
	_ = os.WriteFile(filepath.Join(hcHome, ".config", "base-config.json"), []byte(`{"source": "harness-config"}`), 0644)

	// Create agnostic template (no home, just points to harness-config)
	tplDir := filepath.Join(globalScionDir, "templates", "base-test")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("default_harness_config: test-hc\n"), 0644)

	agentHome, _, _, err := ProvisionAgent(context.Background(), "base-agent", "base-test", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify harness-config home files were copied
	data, err := os.ReadFile(filepath.Join(agentHome, "base-file.txt"))
	if err != nil {
		t.Fatalf("expected base-file.txt in agent home: %v", err)
	}
	if string(data) != "from-harness-config" {
		t.Errorf("expected content 'from-harness-config', got %q", string(data))
	}

	data, err = os.ReadFile(filepath.Join(agentHome, ".config", "base-config.json"))
	if err != nil {
		t.Fatalf("expected .config/base-config.json in agent home: %v", err)
	}
	if !strings.Contains(string(data), "harness-config") {
		t.Errorf("expected base-config.json to contain 'harness-config', got %q", string(data))
	}
}

func TestComposition_TemplateOverlay(t *testing.T) {
	_, globalScionDir, projectScionDir := setupCompositionTest(t)

	// Create harness-config with home files
	hcDir := filepath.Join(globalScionDir, "harness-configs", "overlay-hc")
	hcHome := filepath.Join(hcDir, "home")
	_ = os.MkdirAll(hcHome, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\n"), 0644)
	_ = os.WriteFile(filepath.Join(hcHome, "shared-file.txt"), []byte("from-harness-config"), 0644)
	_ = os.WriteFile(filepath.Join(hcHome, "base-only.txt"), []byte("base-only-content"), 0644)

	// Create template with home directory that overlays
	tplDir := filepath.Join(globalScionDir, "templates", "overlay-test")
	tplHome := filepath.Join(tplDir, "home")
	_ = os.MkdirAll(tplHome, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("default_harness_config: overlay-hc\n"), 0644)
	_ = os.WriteFile(filepath.Join(tplHome, "shared-file.txt"), []byte("from-template"), 0644) // overlay
	_ = os.WriteFile(filepath.Join(tplHome, "template-only.txt"), []byte("template-only-content"), 0644)

	agentHome, _, _, err := ProvisionAgent(context.Background(), "overlay-agent", "overlay-test", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// shared-file.txt should have template content (template wins on conflict)
	data, err := os.ReadFile(filepath.Join(agentHome, "shared-file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "from-template" {
		t.Errorf("expected template overlay to win, got %q", string(data))
	}

	// base-only.txt should exist (from harness-config)
	data, err = os.ReadFile(filepath.Join(agentHome, "base-only.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "base-only-content" {
		t.Errorf("expected base-only.txt content from harness-config, got %q", string(data))
	}

	// template-only.txt should exist (from template)
	data, err = os.ReadFile(filepath.Join(agentHome, "template-only.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "template-only-content" {
		t.Errorf("expected template-only.txt content, got %q", string(data))
	}
}

func TestComposition_AgentInstructionsInjection(t *testing.T) {
	_, globalScionDir, projectScionDir := setupCompositionTest(t)

	seedTestHarnessConfig(t, globalScionDir, "claude-hc", "claude")

	t.Run("inline content", func(t *testing.T) {
		tplDir := filepath.Join(globalScionDir, "templates", "inline-instructions")
		_ = os.MkdirAll(tplDir, 0755)
		tplConfig := `default_harness_config: claude-hc
agent_instructions: "You are a helpful coding assistant."
`
		_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte(tplConfig), 0644)

		agentHome, _, _, err := ProvisionAgent(context.Background(), "inline-agent", "inline-instructions", "", "", projectScionDir, "", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent failed: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(agentHome, ".claude", "CLAUDE.md"))
		if err != nil {
			t.Fatalf("expected CLAUDE.md to exist: %v", err)
		}
		if !strings.Contains(string(data), "helpful coding assistant") {
			t.Errorf("expected agent instructions content, got %q", string(data))
		}
	})

	t.Run("file reference", func(t *testing.T) {
		tplDir := filepath.Join(globalScionDir, "templates", "file-instructions")
		_ = os.MkdirAll(tplDir, 0755)
		_ = os.WriteFile(filepath.Join(tplDir, "my-instructions.md"), []byte("Instructions from file."), 0644)
		tplConfig := `default_harness_config: claude-hc
agent_instructions: my-instructions.md
`
		_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte(tplConfig), 0644)

		agentHome, _, _, err := ProvisionAgent(context.Background(), "file-instr-agent", "file-instructions", "", "", projectScionDir, "", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent failed: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(agentHome, ".claude", "CLAUDE.md"))
		if err != nil {
			t.Fatalf("expected CLAUDE.md to exist: %v", err)
		}
		if !strings.Contains(string(data), "Instructions from file.") {
			t.Errorf("expected content from file in agent instructions, got %q", string(data))
		}
	})
}

func TestComposition_SystemPromptInjection(t *testing.T) {
	_, globalScionDir, projectScionDir := setupCompositionTest(t)

	seedTestHarnessConfig(t, globalScionDir, "claude-hc", "claude")

	tplDir := filepath.Join(globalScionDir, "templates", "sysprompt-test")
	_ = os.MkdirAll(tplDir, 0755)
	tplConfig := `default_harness_config: claude-hc
system_prompt: "Be concise and precise."
`
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte(tplConfig), 0644)

	agentHome, _, _, err := ProvisionAgent(context.Background(), "sysprompt-agent", "sysprompt-test", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Claude harness writes system prompt to .claude/system-prompt.md
	data, err := os.ReadFile(filepath.Join(agentHome, ".claude", "system-prompt.md"))
	if err != nil {
		t.Fatalf("expected system-prompt.md to exist: %v", err)
	}
	if !strings.Contains(string(data), "concise and precise") {
		t.Errorf("expected system prompt content, got %q", string(data))
	}
}

func TestComposition_CommonFiles(t *testing.T) {
	_, globalScionDir, projectScionDir := setupCompositionTest(t)

	seedTestHarnessConfig(t, globalScionDir, "common-hc", "claude")

	tplDir := filepath.Join(globalScionDir, "templates", "common-test")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("default_harness_config: common-hc\n"), 0644)

	agentHome, _, _, err := ProvisionAgent(context.Background(), "common-agent", "common-test", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify common files are present
	if _, err := os.Stat(filepath.Join(agentHome, ".tmux.conf")); os.IsNotExist(err) {
		t.Error("expected .tmux.conf to be seeded in agent home")
	}
	if _, err := os.Stat(filepath.Join(agentHome, ".zshrc")); os.IsNotExist(err) {
		t.Error("expected .zshrc to be seeded in agent home")
	}
}

func TestComposition_HarnessConfigResolution(t *testing.T) {
	_, globalScionDir, projectScionDir := setupCompositionTest(t)

	// Create harness-configs
	seedTestHarnessConfig(t, globalScionDir, "cli-hc", "claude")
	seedTestHarnessConfig(t, globalScionDir, "tpl-hc", "claude")
	seedTestHarnessConfig(t, globalScionDir, "profile-hc", "claude")
	seedTestHarnessConfig(t, globalScionDir, "settings-hc", "claude")

	// Create template with default_harness_config
	tplDir := filepath.Join(globalScionDir, "templates", "resolve-test")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("default_harness_config: tpl-hc\n"), 0644)

	// Create template without default_harness_config (for fallback tests)
	tplDirNoDefault := filepath.Join(globalScionDir, "templates", "no-default-test")
	_ = os.MkdirAll(tplDirNoDefault, 0755)
	_ = os.WriteFile(filepath.Join(tplDirNoDefault, "scion-agent.yaml"), []byte("env:\n  FOO: bar\n"), 0644)

	t.Run("CLI flag wins over template", func(t *testing.T) {
		_, _, cfg, err := ProvisionAgent(context.Background(), "cli-wins", "resolve-test", "", "cli-hc", projectScionDir, "", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent failed: %v", err)
		}
		if cfg.HarnessConfig != "cli-hc" {
			t.Errorf("expected HarnessConfig = 'cli-hc', got %q", cfg.HarnessConfig)
		}
	})

	t.Run("template default used when no CLI flag", func(t *testing.T) {
		_, _, cfg, err := ProvisionAgent(context.Background(), "tpl-default", "resolve-test", "", "", projectScionDir, "", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent failed: %v", err)
		}
		if cfg.HarnessConfig != "tpl-hc" {
			t.Errorf("expected HarnessConfig = 'tpl-hc', got %q", cfg.HarnessConfig)
		}
	})

	t.Run("profile default used when template has none", func(t *testing.T) {
		// Write settings with profile that has default_harness_config
		settingsYAML := `schema_version: "1"
profiles:
  test-profile:
    runtime: docker
    default_harness_config: profile-hc
`
		_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settingsYAML), 0644)

		_, _, cfg, err := ProvisionAgent(context.Background(), "profile-default", "no-default-test", "", "", projectScionDir, "test-profile", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent failed: %v", err)
		}
		if cfg.HarnessConfig != "profile-hc" {
			t.Errorf("expected HarnessConfig = 'profile-hc', got %q", cfg.HarnessConfig)
		}
		// Clean up settings
		_ = os.Remove(filepath.Join(globalScionDir, "settings.yaml"))
	})

	t.Run("top-level settings default used as last resort", func(t *testing.T) {
		settingsYAML := `schema_version: "1"
default_harness_config: settings-hc
`
		_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settingsYAML), 0644)

		_, _, cfg, err := ProvisionAgent(context.Background(), "settings-default", "no-default-test", "", "", projectScionDir, "", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent failed: %v", err)
		}
		if cfg.HarnessConfig != "settings-hc" {
			t.Errorf("expected HarnessConfig = 'settings-hc', got %q", cfg.HarnessConfig)
		}
		_ = os.Remove(filepath.Join(globalScionDir, "settings.yaml"))
	})

	t.Run("error when no harness-config resolved", func(t *testing.T) {
		_, _, _, err := ProvisionAgent(context.Background(), "no-hc", "no-default-test", "", "", projectScionDir, "", "", "", "")
		if err == nil {
			t.Fatal("expected error when no harness-config can be resolved")
		}
		if !strings.Contains(err.Error(), "no harness-config resolved") {
			t.Errorf("expected error about harness-config resolution, got: %v", err)
		}
	})
}

func TestComposition_LegacyTemplateRejected(t *testing.T) {
	_, globalScionDir, projectScionDir := setupCompositionTest(t)

	// Create a legacy template with harness field
	tplDir := filepath.Join(globalScionDir, "templates", "legacy-tpl")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("harness: claude\n"), 0644)

	_, _, _, err := ProvisionAgent(context.Background(), "legacy-agent", "legacy-tpl", "", "", projectScionDir, "", "", "", "")
	if err == nil {
		t.Fatal("expected error for legacy template with 'harness' field")
	}
	if !strings.Contains(err.Error(), "harness") {
		t.Errorf("expected error to mention 'harness', got: %v", err)
	}
}

func TestComposition_HarnessConfigPersistedInAgentInfo(t *testing.T) {
	tmpDir, globalScionDir, projectScionDir := setupCompositionTest(t)

	seedTestHarnessConfig(t, globalScionDir, "persist-hc", "claude")

	tplDir := filepath.Join(globalScionDir, "templates", "persist-test")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("default_harness_config: persist-hc\n"), 0644)

	agentHome, _, cfg, err := ProvisionAgent(context.Background(), "persist-agent", "persist-test", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify harness-config persisted in returned config
	if cfg.HarnessConfig != "persist-hc" {
		t.Errorf("expected HarnessConfig = 'persist-hc', got %q", cfg.HarnessConfig)
	}

	// Verify agent-info.json has harness-config
	infoData, err := os.ReadFile(filepath.Join(agentHome, "agent-info.json"))
	if err != nil {
		t.Fatalf("failed to read agent-info.json: %v", err)
	}
	if !strings.Contains(string(infoData), "persist-hc") {
		t.Errorf("expected agent-info.json to contain 'persist-hc', got: %s", string(infoData))
	}

	_ = tmpDir
}

// setupInlineHarnessTemplate creates a template with an inline harness-config
// that provides home/.claude/claude.md (lowercase) and a template-level agents.md.
// If agentInstructions is empty, agent_instructions is omitted from the config.
func setupInlineHarnessTemplate(t *testing.T, scionDir, tplName, agentInstructions string) string {
	t.Helper()
	tplDir := filepath.Join(scionDir, "templates", tplName)
	_ = os.MkdirAll(tplDir, 0755)

	// Template-level agents.md (the instruction file that should win)
	_ = os.WriteFile(filepath.Join(tplDir, "agents.md"), []byte("# Template Agent Instructions\nThese are from the template."), 0644)
	_ = os.WriteFile(filepath.Join(tplDir, "system-prompt.md"), []byte("Be helpful."), 0644)

	// Template scion-agent.yaml
	tplConfig := "default_harness_config: claude-web\n"
	if agentInstructions != "" {
		tplConfig += "agent_instructions: " + agentInstructions + "\n"
	}
	tplConfig += "system_prompt: system-prompt.md\n"
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte(tplConfig), 0644)

	// Inline harness-config: harness-configs/claude-web/
	hcDir := filepath.Join(tplDir, "harness-configs", "claude-web")
	hcHome := filepath.Join(hcDir, "home")
	_ = os.MkdirAll(filepath.Join(hcHome, ".claude", "skills", "chrome-devtools"), 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nimage: test-claude:latest\nskills_dir: .claude/skills\ninstructions_file: .claude/CLAUDE.md\n"), 0644)

	// Harness config provides claude.md (lowercase) — this should be REPLACED
	_ = os.WriteFile(filepath.Join(hcHome, ".claude", "claude.md"), []byte("# Harness Config Instructions\nThese are from the harness config."), 0644)
	_ = os.WriteFile(filepath.Join(hcHome, ".claude", "settings.json"), []byte(`{"theme": "dark"}`), 0644)
	_ = os.WriteFile(filepath.Join(hcHome, ".claude", "skills", "chrome-devtools", "SKILL.md"), []byte("# Chrome DevTools"), 0644)
	_ = os.WriteFile(filepath.Join(hcHome, ".claude.json"), []byte(`{"projects": {}}`), 0644)
	_ = os.WriteFile(filepath.Join(hcHome, ".bashrc"), []byte("# bashrc"), 0644)
	_ = os.WriteFile(filepath.Join(hcHome, ".tmux.conf"), []byte("# tmux"), 0644)

	return tplDir
}

// assertClaudeInstructions checks that the agent home has CLAUDE.md with template
// instructions and that the lowercase harness-config claude.md has been replaced.
func assertClaudeInstructions(t *testing.T, agentHome string) {
	t.Helper()
	claudeDir := filepath.Join(agentHome, ".claude")

	// The canonical CLAUDE.md should exist with template instructions
	canonicalPath := filepath.Join(claudeDir, "CLAUDE.md")
	data, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("expected CLAUDE.md at %s: %v", canonicalPath, err)
	}
	if !strings.Contains(string(data), "Template Agent Instructions") {
		t.Errorf("CLAUDE.md should contain template instructions, got %q", string(data))
	}
	if strings.Contains(string(data), "Harness Config Instructions") {
		t.Errorf("CLAUDE.md should NOT contain harness config instructions, got %q", string(data))
	}

	// Lowercase claude.md should NOT exist (on case-sensitive FS)
	entries, _ := os.ReadDir(claudeDir)
	for _, e := range entries {
		if strings.EqualFold(e.Name(), "CLAUDE.md") && e.Name() != "CLAUDE.md" {
			t.Errorf("stale lowercase %q should have been removed from %s", e.Name(), claudeDir)
		}
	}

	// Other harness config files should still be present
	if _, err := os.Stat(filepath.Join(claudeDir, "settings.json")); os.IsNotExist(err) {
		t.Error("expected settings.json to still exist from harness config")
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "skills", "chrome-devtools", "SKILL.md")); os.IsNotExist(err) {
		t.Error("expected SKILL.md to still exist from harness config")
	}
}

func TestComposition_InlineHarnessConfigWithAgentInstructions(t *testing.T) {
	// Template explicitly sets agent_instructions: agents.md in scion-agent.yaml.
	// Inline harness-config provides home/.claude/claude.md (lowercase).
	// Expected: .claude/CLAUDE.md should contain the template's agents.md content.
	_, globalScionDir, projectScionDir := setupCompositionTest(t)
	setupInlineHarnessTemplate(t, globalScionDir, "web-dev-explicit", "agents.md")

	agentHome, _, _, err := ProvisionAgent(context.Background(), "explicit-instruct", "web-dev-explicit", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	assertClaudeInstructions(t, agentHome)
}

func TestComposition_InlineHarnessConfigAutoDetectsAgentsMd(t *testing.T) {
	// Template has agents.md but does NOT set agent_instructions in config.
	// Inline harness-config provides home/.claude/claude.md (lowercase).
	// Expected: auto-detection should find agents.md and inject it,
	// replacing the harness-config's claude.md.
	_, globalScionDir, projectScionDir := setupCompositionTest(t)
	setupInlineHarnessTemplate(t, globalScionDir, "web-dev-auto", "") // no agent_instructions

	agentHome, _, _, err := ProvisionAgent(context.Background(), "auto-instruct", "web-dev-auto", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	assertClaudeInstructions(t, agentHome)
}

func TestComposition_FullInitProjectFlow(t *testing.T) {
	mockRuntimeForTest(t)
	// End-to-end test: InitProject + default template + harness-config resolution
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	// InitMachine seeds global harness-configs (required for agent creation)
	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := config.InitProject(projectScionDir, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	_ = os.Chdir(projectDir)

	// Use the "default" template (agnostic); default_harness_config: claude comes from settings
	agentHome, _, cfg, err := ProvisionAgent(context.Background(), "full-flow-agent", "default", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify harness was resolved from claude harness-config
	if cfg.Harness != "claude" {
		t.Errorf("expected harness 'claude', got %q", cfg.Harness)
	}

	// Verify harness-config name is set
	if cfg.HarnessConfig != "claude" {
		t.Errorf("expected HarnessConfig 'claude', got %q", cfg.HarnessConfig)
	}

	// Verify agent home exists
	if agentHome == "" {
		t.Fatal("expected non-empty agent home path")
	}

	// Verify common files present
	if _, err := os.Stat(filepath.Join(agentHome, ".tmux.conf")); os.IsNotExist(err) {
		t.Error("expected .tmux.conf in agent home")
	}
}
