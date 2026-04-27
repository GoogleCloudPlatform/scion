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

func TestLoadHarnessConfigDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a valid harness-config directory
	configDir := filepath.Join(tmpDir, "claude")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}

	configYAML := `harness: claude
image: scion-claude:latest
user: scion
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatal(err)
	}

	hc, err := LoadHarnessConfigDir(configDir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir failed: %v", err)
	}

	if hc.Name != "claude" {
		t.Errorf("expected name 'claude', got %q", hc.Name)
	}
	if hc.Config.Harness != "claude" {
		t.Errorf("expected harness 'claude', got %q", hc.Config.Harness)
	}
	if hc.Config.Image != "scion-claude:latest" {
		t.Errorf("expected image to be set, got %q", hc.Config.Image)
	}
	if hc.Config.User != "scion" {
		t.Errorf("expected user 'scion', got %q", hc.Config.User)
	}
}

func TestLoadHarnessConfigDir_ExtendedFields(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "claude")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	configYAML := `harness: claude
image: scion-claude:latest
user: scion
provisioner:
  type: builtin
  interface_version: 1
config_dir: .claude
skills_dir: .claude/skills
interrupt_key: Escape
command:
  base: ["claude"]
  resume_flag: "--continue"
capabilities:
  limits:
    max_turns: { support: "yes" }
auth:
  default_type: api-key
  types:
    api-key:
      required_env:
        - any_of: ["ANTHROPIC_API_KEY"]
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatal(err)
	}

	hc, err := LoadHarnessConfigDir(configDir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir failed: %v", err)
	}
	if hc.Config.Provisioner == nil || hc.Config.Provisioner.Type != "builtin" {
		t.Fatalf("expected provisioner builtin, got %#v", hc.Config.Provisioner)
	}
	if hc.Config.Command == nil || len(hc.Config.Command.Base) != 1 || hc.Config.Command.Base[0] != "claude" {
		t.Fatalf("expected command metadata to load, got %#v", hc.Config.Command)
	}
	if hc.Config.Auth == nil || hc.Config.Auth.DefaultType != "api-key" {
		t.Fatalf("expected auth metadata to load, got %#v", hc.Config.Auth)
	}
}

func TestLoadHarnessConfigDir_InvalidUnknownField(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "claude")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("harness: claude\nunknown: true\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHarnessConfigDir(configDir); err == nil {
		t.Fatal("expected invalid harness config to fail validation")
	}
}

func TestLoadHarnessConfigDir_NotFound(t *testing.T) {
	_, err := LoadHarnessConfigDir("/nonexistent/path")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestLoadHarnessConfigDir_MissingConfigYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "empty")
	os.MkdirAll(configDir, 0755)

	_, err := LoadHarnessConfigDir(configDir)
	if err == nil {
		t.Fatal("expected error for missing config.yaml")
	}
}

func TestFindHarnessConfigDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Setup grove-level harness-config
	grovePath := filepath.Join(tmpDir, "grove")
	groveHCDir := filepath.Join(grovePath, harnessConfigsDirName, "claude")
	if err := os.MkdirAll(groveHCDir, 0755); err != nil {
		t.Fatal(err)
	}
	groveConfigYAML := `harness: claude
image: grove-image:latest
user: scion
`
	if err := os.WriteFile(filepath.Join(groveHCDir, "config.yaml"), []byte(groveConfigYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Override HOME so GetGlobalDir resolves to our temp dir
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Test: grove-level takes precedence
	hc, err := FindHarnessConfigDir("claude", grovePath)
	if err != nil {
		t.Fatalf("FindHarnessConfigDir failed: %v", err)
	}
	if hc.Config.Image != "grove-image:latest" {
		t.Errorf("expected grove-level image, got %q", hc.Config.Image)
	}

	// Setup global harness-config at ~/.scion/harness-configs/gemini/
	globalScionDir := filepath.Join(tmpDir, DotScion, harnessConfigsDirName, "gemini")
	if err := os.MkdirAll(globalScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	geminiConfig := `harness: gemini
image: gemini-global:latest
user: scion
`
	if err := os.WriteFile(filepath.Join(globalScionDir, "config.yaml"), []byte(geminiConfig), 0644); err != nil {
		t.Fatal(err)
	}

	// Test: falls back to global when no grove match
	hc, err = FindHarnessConfigDir("gemini", "")
	if err != nil {
		t.Fatalf("FindHarnessConfigDir for global gemini failed: %v", err)
	}
	if hc.Config.Image != "gemini-global:latest" {
		t.Errorf("expected global image, got %q", hc.Config.Image)
	}

	// Test: not found
	_, err = FindHarnessConfigDir("nonexistent", grovePath)
	if err == nil {
		t.Fatal("expected error for nonexistent harness-config")
	}

	// Test: "generic" returns a synthetic entry even with no on-disk directory
	hc, err = FindHarnessConfigDir("generic", grovePath)
	if err != nil {
		t.Fatalf("FindHarnessConfigDir for generic should succeed: %v", err)
	}
	if hc.Name != "generic" {
		t.Errorf("expected name 'generic', got %q", hc.Name)
	}
	if hc.Config.Harness != "generic" {
		t.Errorf("expected harness 'generic', got %q", hc.Config.Harness)
	}
}

func TestFindHarnessConfigDir_TemplatePaths(t *testing.T) {
	tmpDir := t.TempDir()

	// Override HOME so global dir resolves to our temp dir
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Setup a template with a custom harness-config
	templateDir := filepath.Join(tmpDir, "templates", "web-dev")
	tplHCDir := filepath.Join(templateDir, harnessConfigsDirName, "claude-web")
	if err := os.MkdirAll(tplHCDir, 0755); err != nil {
		t.Fatal(err)
	}
	tplConfigYAML := `harness: claude
image: claude-web-image:latest
user: scion
`
	if err := os.WriteFile(filepath.Join(tplHCDir, "config.yaml"), []byte(tplConfigYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Test: template-only harness-config is found
	hc, err := FindHarnessConfigDir("claude-web", "", templateDir)
	if err != nil {
		t.Fatalf("FindHarnessConfigDir with template path failed: %v", err)
	}
	if hc.Name != "claude-web" {
		t.Errorf("expected name 'claude-web', got %q", hc.Name)
	}
	if hc.Config.Image != "claude-web-image:latest" {
		t.Errorf("expected template image, got %q", hc.Config.Image)
	}

	// Test: template harness-config takes precedence over global
	globalHCDir := filepath.Join(tmpDir, DotScion, harnessConfigsDirName, "claude-web")
	if err := os.MkdirAll(globalHCDir, 0755); err != nil {
		t.Fatal(err)
	}
	globalConfigYAML := `harness: claude
image: global-claude-web:latest
user: scion
`
	if err := os.WriteFile(filepath.Join(globalHCDir, "config.yaml"), []byte(globalConfigYAML), 0644); err != nil {
		t.Fatal(err)
	}

	hc, err = FindHarnessConfigDir("claude-web", "", templateDir)
	if err != nil {
		t.Fatalf("FindHarnessConfigDir with template+global failed: %v", err)
	}
	if hc.Config.Image != "claude-web-image:latest" {
		t.Errorf("expected template image to take precedence, got %q", hc.Config.Image)
	}

	// Test: template harness-config takes precedence over grove-level too
	grovePath := filepath.Join(tmpDir, "grove")
	groveHCDir := filepath.Join(grovePath, harnessConfigsDirName, "claude-web")
	if err := os.MkdirAll(groveHCDir, 0755); err != nil {
		t.Fatal(err)
	}
	groveConfigYAML := `harness: claude
image: grove-claude-web:latest
user: scion
`
	if err := os.WriteFile(filepath.Join(groveHCDir, "config.yaml"), []byte(groveConfigYAML), 0644); err != nil {
		t.Fatal(err)
	}

	hc, err = FindHarnessConfigDir("claude-web", grovePath, templateDir)
	if err != nil {
		t.Fatalf("FindHarnessConfigDir with template+grove+global failed: %v", err)
	}
	if hc.Config.Image != "claude-web-image:latest" {
		t.Errorf("expected template image to take precedence over grove, got %q", hc.Config.Image)
	}

	// Test: without template paths, falls back to grove then global
	hc, err = FindHarnessConfigDir("claude-web", grovePath)
	if err != nil {
		t.Fatalf("FindHarnessConfigDir without template path failed: %v", err)
	}
	if hc.Config.Image != "grove-claude-web:latest" {
		t.Errorf("expected grove image without template path, got %q", hc.Config.Image)
	}
}

func TestListHarnessConfigDirs(t *testing.T) {
	tmpDir := t.TempDir()

	// Override HOME
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Setup global harness-configs
	globalBase := filepath.Join(tmpDir, DotScion, harnessConfigsDirName)
	for _, name := range []string{"claude", "gemini"} {
		dir := filepath.Join(globalBase, name)
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("harness: "+name+"\n"), 0644)
	}

	// Setup grove-level harness-config (overrides global claude, adds codex)
	grovePath := filepath.Join(tmpDir, "grove")
	groveBase := filepath.Join(grovePath, harnessConfigsDirName)
	for _, name := range []string{"claude", "codex"} {
		dir := filepath.Join(groveBase, name)
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("harness: "+name+"\nimage: grove-"+name+"\n"), 0644)
	}

	configs, err := ListHarnessConfigDirs(grovePath)
	if err != nil {
		t.Fatalf("ListHarnessConfigDirs failed: %v", err)
	}

	if len(configs) != 3 {
		t.Fatalf("expected 3 configs (claude, codex, gemini), got %d", len(configs))
	}

	// Should be sorted alphabetically
	names := make([]string, len(configs))
	for i, c := range configs {
		names[i] = c.Name
	}
	expected := []string{"claude", "codex", "gemini"}
	for i, name := range expected {
		if names[i] != name {
			t.Errorf("expected configs[%d].Name = %q, got %q", i, name, names[i])
		}
	}

	// Grove claude should override global claude
	for _, c := range configs {
		if c.Name == "claude" && c.Config.Image != "grove-claude" {
			t.Errorf("expected grove-level claude image, got %q", c.Config.Image)
		}
	}
}

func TestSeedHarnessConfig_MockHarness(t *testing.T) {
	tmpDir := t.TempDir()

	// Mock harnesses return empty embed FS, so SeedHarnessConfig
	// should return nil without creating directories (no embeds to seed).
	harnesses := GetMockHarnesses()

	for _, h := range harnesses {
		hcDir := filepath.Join(tmpDir, "hc", h.Name())
		err := SeedHarnessConfig(hcDir, h, false)
		if err != nil {
			t.Errorf("SeedHarnessConfig(%s) failed: %v", h.Name(), err)
		}
	}
}

func TestSeedHarnessConfigFromFS(t *testing.T) {
	tmpDir := t.TempDir()

	// Use the default template's home directory as a source FS to test
	// SeedHarnessConfigFromFS mechanics (directory creation, file copying).
	targetDir := filepath.Join(tmpDir, "test-config")

	err := SeedHarnessConfigFromFS(targetDir, EmbedsFS, "embeds/templates/default/home", ".test-config", false)
	if err != nil {
		t.Fatalf("SeedHarnessConfigFromFS failed: %v", err)
	}

	// Verify directory structure was created
	if _, err := os.Stat(filepath.Join(targetDir, "home")); err != nil {
		t.Error("expected home directory to be created")
	}
	if _, err := os.Stat(filepath.Join(targetDir, "home", ".test-config")); err != nil {
		t.Error("expected config directory to be created")
	}
}

func TestMapEmbedFileToHarnessConfigPath_RootSupportFiles(t *testing.T) {
	targetDir := "/tmp/hc"
	homeDir := filepath.Join(targetDir, "home")
	tests := map[string]string{
		"provision.py":                 filepath.Join(targetDir, "provision.py"),
		"dialect.yaml":                 filepath.Join(targetDir, "dialect.yaml"),
		"schema/manifest.json":         filepath.Join(targetDir, "schema", "manifest.json"),
		"examples/basic.json":          filepath.Join(targetDir, "examples", "basic.json"),
		"tests/fixtures/manifest.json": filepath.Join(targetDir, "tests", "fixtures", "manifest.json"),
		"home/.tool/settings.json":     filepath.Join(homeDir, ".tool", "settings.json"),
		"settings.json":                filepath.Join(homeDir, ".tool", "settings.json"),
	}
	for input, want := range tests {
		if got := mapEmbedFileToHarnessConfigPath(targetDir, homeDir, ".tool", input); got != want {
			t.Errorf("mapEmbedFileToHarnessConfigPath(%q) = %q, want %q", input, got, want)
		}
	}
}
