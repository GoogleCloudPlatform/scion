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

	"github.com/ptone/scion-agent/pkg/api"
)

func TestCreateTemplate(t *testing.T) {
	// Setup a temporary directory for the test
	tmpDir, err := os.MkdirTemp("", "scion-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Override home dir for global templates
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Create a mock project structure
	projectDir := filepath.Join(tmpDir, "project", DotScion)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Helper to change current working directory
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	if err := os.Chdir(filepath.Dir(projectDir)); err != nil {
		t.Fatal(err)
	}

	// Test creating a project template
	tplName := "test-tpl"

	err = CreateTemplate(tplName, false)
	if err != nil {
		t.Fatalf("failed to create project template: %v", err)
	}

	expectedPath := filepath.Join(projectDir, "templates", tplName)
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Errorf("expected template directory %s to exist", expectedPath)
	}

	// Verify key agnostic template files exist
	files := []string{
		"scion-agent.yaml",
		"agents.md",
		"system-prompt.md",
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(expectedPath, f)); os.IsNotExist(err) {
			t.Errorf("expected file %s to exist in template", f)
		}
	}

	// Test creating a global template
	globalTplName := "global-tpl"
	err = CreateTemplate(globalTplName, true)
	if err != nil {
		t.Fatalf("failed to create global template: %v", err)
	}

	globalExpectedPath := filepath.Join(tmpDir, GlobalDir, "templates", globalTplName)
	if _, err := os.Stat(globalExpectedPath); os.IsNotExist(err) {
		t.Errorf("expected global template directory %s to exist", globalExpectedPath)
	}

	// Test duplicate template creation fails
	err = CreateTemplate(tplName, false)
	if err == nil {
		t.Error("expected error when creating duplicate template, got nil")
	}
}

func TestDeleteTemplate(t *testing.T) {
	// Setup a temporary directory for the test
	tmpDir, err := os.MkdirTemp("", "scion-test-delete-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Override home dir for global templates
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Create a mock project structure
	projectDir := filepath.Join(tmpDir, "project", DotScion)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Helper to change current working directory
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	if err := os.Chdir(filepath.Dir(projectDir)); err != nil {
		t.Fatal(err)
	}

	// Create templates to delete
	tplName := "test-tpl-delete"

	if err := CreateTemplate(tplName, false); err != nil {
		t.Fatal(err)
	}
	globalTplName := "global-tpl-delete"
	if err := CreateTemplate(globalTplName, true); err != nil {
		t.Fatal(err)
	}

	// Test deleting project template
	if err := DeleteTemplate(tplName, false); err != nil {
		t.Fatalf("failed to delete project template: %v", err)
	}
	expectedPath := filepath.Join(projectDir, "templates", tplName)
	if _, err := os.Stat(expectedPath); !os.IsNotExist(err) {
		t.Errorf("expected template directory %s to be gone", expectedPath)
	}

	// Test deleting global template
	if err := DeleteTemplate(globalTplName, true); err != nil {
		t.Fatalf("failed to delete global template: %v", err)
	}
	globalExpectedPath := filepath.Join(tmpDir, GlobalDir, "templates", globalTplName)
	if _, err := os.Stat(globalExpectedPath); !os.IsNotExist(err) {
		t.Errorf("expected global template directory %s to be gone", globalExpectedPath)
	}

	// Test deleting "gemini" fails
	if err := DeleteTemplate("gemini", false); err == nil {
		t.Error("expected error when deleting gemini template, got nil")
	}

	// Test deleting non-existent template fails
	if err := DeleteTemplate("no-such-template", false); err == nil {
		t.Error("expected error when deleting non-existent template, got nil")
	}
}

func TestUpdateDefaultTemplates(t *testing.T) {
	// Setup a temporary directory for the test
	tmpDir, err := os.MkdirTemp("", "scion-test-update-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Override home dir
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Create a mock project structure
	projectDir := filepath.Join(tmpDir, "project", DotScion)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Helper to change current working directory
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	if err := os.Chdir(filepath.Dir(projectDir)); err != nil {
		t.Fatal(err)
	}

	// Initialize project (creates default agnostic template)
	if err := InitProject("", GetMockHarnesses()); err != nil {
		t.Fatal(err)
	}

	defaultScionYAML := filepath.Join(projectDir, "templates", "default", "scion-agent.yaml")

	// Corrupt the default template file
	corruptContent := "CORRUPT"
	if err := os.WriteFile(defaultScionYAML, []byte(corruptContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Update default templates
	if err := UpdateDefaultTemplates(false, GetMockHarnesses()); err != nil {
		t.Fatalf("failed to update default templates: %v", err)
	}

	// Verify the default agnostic template was restored
	data, err := os.ReadFile(defaultScionYAML)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == corruptContent {
		t.Error("expected scion-agent.yaml to be overwritten, but it still contains corrupt content")
	}
}

func TestMergeScionConfig(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name     string
		base     *api.ScionConfig
		override *api.ScionConfig
		wantStatus string
	}{
		{
			name:     "override status",
			base:     &api.ScionConfig{Info: &api.AgentInfo{Status: "created"}},
			override: &api.ScionConfig{Info: &api.AgentInfo{Status: "running"}},
			wantStatus: "running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeScionConfig(tt.base, tt.override)
			if got.Info == nil || got.Info.Status != tt.wantStatus {
				t.Errorf("MergeScionConfig() Status = %v, want %v", got.Info.Status, tt.wantStatus)
			}
		})
	}

	t.Run("model merge", func(t *testing.T) {
		base := &api.ScionConfig{Model: "flash"}
		override := &api.ScionConfig{Model: "pro"}
		got := MergeScionConfig(base, override)
		if got.Model != "pro" {
			t.Errorf("expected model to be pro, got %v", got.Model)
		}
	})

	t.Run("detached merge", func(t *testing.T) {
		base := &api.ScionConfig{Detached: &trueVal}
		override := &api.ScionConfig{Detached: &falseVal}
		got := MergeScionConfig(base, override)
		if got.Detached == nil || *got.Detached != false {
			t.Errorf("expected detached to be false, got %v", got.Detached)
		}
	})

	t.Run("max_turns override replaces base", func(t *testing.T) {
		base := &api.ScionConfig{MaxTurns: 10}
		override := &api.ScionConfig{MaxTurns: 50}
		got := MergeScionConfig(base, override)
		if got.MaxTurns != 50 {
			t.Errorf("expected MaxTurns=50, got %d", got.MaxTurns)
		}
	})

	t.Run("max_turns zero override keeps base", func(t *testing.T) {
		base := &api.ScionConfig{MaxTurns: 10}
		override := &api.ScionConfig{}
		got := MergeScionConfig(base, override)
		if got.MaxTurns != 10 {
			t.Errorf("expected MaxTurns=10, got %d", got.MaxTurns)
		}
	})

	t.Run("max_duration override replaces base", func(t *testing.T) {
		base := &api.ScionConfig{MaxDuration: "1h"}
		override := &api.ScionConfig{MaxDuration: "2h"}
		got := MergeScionConfig(base, override)
		if got.MaxDuration != "2h" {
			t.Errorf("expected MaxDuration=2h, got %s", got.MaxDuration)
		}
	})

	t.Run("max_duration empty override keeps base", func(t *testing.T) {
		base := &api.ScionConfig{MaxDuration: "1h"}
		override := &api.ScionConfig{}
		got := MergeScionConfig(base, override)
		if got.MaxDuration != "1h" {
			t.Errorf("expected MaxDuration=1h, got %s", got.MaxDuration)
		}
	})
}

func TestCloneTemplate(t *testing.T) {
	// Setup a temporary directory for the test
	tmpDir, err := os.MkdirTemp("", "scion-test-clone-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Override home dir for global templates
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Create a mock project structure
	projectDir := filepath.Join(tmpDir, "project", DotScion)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Helper to change current working directory
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	if err := os.Chdir(filepath.Dir(projectDir)); err != nil {
		t.Fatal(err)
	}

	// Create a source template
	srcName := "src-tpl"

	if err := CreateTemplate(srcName, false); err != nil {
		t.Fatal(err)
	}

	// Test cloning to project
	destName := "dest-tpl"
	if err := CloneTemplate(srcName, destName, false); err != nil {
		t.Fatalf("failed to clone template: %v", err)
	}

	expectedPath := filepath.Join(projectDir, "templates", destName)
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Errorf("expected cloned template directory %s to exist", expectedPath)
	}

	// Verify key agnostic template files exist in destination
	files := []string{
		"scion-agent.yaml",
		"agents.md",
		"system-prompt.md",
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(expectedPath, f)); os.IsNotExist(err) {
			t.Errorf("expected file %s to exist in cloned template", f)
		}
	}

	// Test cloning to global
	globalDestName := "global-dest-tpl"
	if err := CloneTemplate(srcName, globalDestName, true); err != nil {
		t.Fatalf("failed to clone template to global: %v", err)
	}

	globalExpectedPath := filepath.Join(tmpDir, GlobalDir, "templates", globalDestName)
	if _, err := os.Stat(globalExpectedPath); os.IsNotExist(err) {
		t.Errorf("expected global cloned template directory %s to exist", globalExpectedPath)
	}

	// Test cloning non-existent template fails
	if err := CloneTemplate("no-such-template", "should-fail", false); err == nil {
		t.Error("expected error when cloning non-existent template, got nil")
	}

	// Test cloning to existing destination fails
	if err := CloneTemplate(srcName, destName, false); err == nil {
		t.Error("expected error when cloning to existing destination, got nil")
	}
}

func TestLoadConfigInvalidVolumes(t *testing.T) {
	t.Run("volumes as object instead of array", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "scion-test-invalid-volumes-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(tmpDir)

		// Write a config where volumes is an object instead of an array
		configContent := `{
			"harness": "gemini",
			"volumes": {"source": "/foo", "target": "/bar"}
		}`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.json"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}

		tpl := &Template{Path: tmpDir}
		_, err = tpl.LoadConfig()
		if err == nil {
			t.Fatal("LoadConfig() expected error for volumes as object, got nil")
		}
		// Should fail at JSON parse level since volumes expects an array
		t.Logf("Got expected error: %v", err)
	})

	t.Run("volume missing target", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "scion-test-invalid-volumes-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(tmpDir)

		configContent := `{
			"harness": "gemini",
			"volumes": [{"source": "/foo"}]
		}`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.json"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}

		tpl := &Template{Path: tmpDir}
		_, err = tpl.LoadConfig()
		if err == nil {
			t.Fatal("LoadConfig() expected error for volume missing target, got nil")
		}
		if !strings.Contains(err.Error(), "missing required field: target") {
			t.Errorf("LoadConfig() error = %q, want containing 'missing required field: target'", err.Error())
		}
	})

	t.Run("volume with invalid type", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "scion-test-invalid-volumes-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(tmpDir)

		configContent := `{
			"harness": "gemini",
			"volumes": [{"source": "/foo", "target": "/bar", "type": "nfs"}]
		}`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.json"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}

		tpl := &Template{Path: tmpDir}
		_, err = tpl.LoadConfig()
		if err == nil {
			t.Fatal("LoadConfig() expected error for invalid volume type, got nil")
		}
		if !strings.Contains(err.Error(), "invalid type") {
			t.Errorf("LoadConfig() error = %q, want containing 'invalid type'", err.Error())
		}
	})
}

func TestFindTemplateInGrovePath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "scion-test-grove-path-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Override HOME for global templates
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Set CWD to tmpDir so CWD-based resolution won't find any .scion
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	os.Chdir(tmpDir)

	// Create a global template
	globalTemplatesDir := filepath.Join(tmpDir, GlobalDir, "templates")
	globalTplDir := filepath.Join(globalTemplatesDir, "my-tpl")
	if err := os.MkdirAll(globalTplDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a grove with its own template
	grovePath := filepath.Join(tmpDir, "some-project", DotScion)
	groveTemplatesDir := filepath.Join(grovePath, "templates")
	groveTplDir := filepath.Join(groveTemplatesDir, "my-tpl")
	if err := os.MkdirAll(groveTplDir, 0755); err != nil {
		t.Fatal(err)
	}

	t.Run("grove template found when grovePath is provided", func(t *testing.T) {
		tpl, err := FindTemplateInGrovePath("my-tpl", grovePath)
		if err != nil {
			t.Fatalf("FindTemplateInGrovePath failed: %v", err)
		}
		if tpl.Path != groveTplDir {
			t.Errorf("expected path %q, got %q", groveTplDir, tpl.Path)
		}
		if tpl.Scope != "grove" {
			t.Errorf("expected scope 'grove', got %q", tpl.Scope)
		}
	})

	t.Run("falls back to global when grove has no template", func(t *testing.T) {
		tpl, err := FindTemplateInGrovePath("my-tpl", filepath.Join(tmpDir, "empty-grove"))
		if err != nil {
			t.Fatalf("FindTemplateInGrovePath failed: %v", err)
		}
		if tpl.Path != globalTplDir {
			t.Errorf("expected path %q, got %q", globalTplDir, tpl.Path)
		}
		if tpl.Scope != "global" {
			t.Errorf("expected scope 'global', got %q", tpl.Scope)
		}
	})

	t.Run("falls back to FindTemplate when grovePath is empty", func(t *testing.T) {
		// With empty grovePath and CWD having no .scion, should fall back to global
		tpl, err := FindTemplateInGrovePath("my-tpl", "")
		if err != nil {
			t.Fatalf("FindTemplateInGrovePath failed: %v", err)
		}
		if tpl.Path != globalTplDir {
			t.Errorf("expected path %q, got %q", globalTplDir, tpl.Path)
		}
	})

	t.Run("returns error when template not found anywhere", func(t *testing.T) {
		_, err := FindTemplateInGrovePath("nonexistent", grovePath)
		if err == nil {
			t.Fatal("expected error for nonexistent template, got nil")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("expected error to contain 'not found', got: %v", err)
		}
	})

	t.Run("absolute path bypasses grove resolution", func(t *testing.T) {
		tpl, err := FindTemplateInGrovePath(globalTplDir, grovePath)
		if err != nil {
			t.Fatalf("FindTemplateInGrovePath failed: %v", err)
		}
		if tpl.Path != globalTplDir {
			t.Errorf("expected path %q, got %q", globalTplDir, tpl.Path)
		}
	})
}

func TestGetTemplateChainInGrove(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "scion-test-chain-grove-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	os.Chdir(tmpDir)

	// Create grove template
	grovePath := filepath.Join(tmpDir, "project", DotScion)
	groveTplDir := filepath.Join(grovePath, "templates", "test-tpl")
	if err := os.MkdirAll(groveTplDir, 0755); err != nil {
		t.Fatal(err)
	}

	chain, err := GetTemplateChainInGrove("test-tpl", grovePath)
	if err != nil {
		t.Fatalf("GetTemplateChainInGrove failed: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("expected chain length 1, got %d", len(chain))
	}
	if chain[0].Path != groveTplDir {
		t.Errorf("expected path %q, got %q", groveTplDir, chain[0].Path)
	}
}

func TestImageFieldLoadingAndMerging(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "scion-test-image-field")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// 1. Test LoadConfig
	configContent := `{
		"image": "custom-image:v1",
		"harness": "test-harness"
	}`
	configPath := filepath.Join(tmpDir, "scion-agent.json")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	tpl := &Template{Path: tmpDir}
	cfg, err := tpl.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Image != "custom-image:v1" {
		t.Errorf("expected Image to be 'custom-image:v1', got '%s'", cfg.Image)
	}

	// 2. Test MergeScionConfig
	base := &api.ScionConfig{
		Image: "base-image:v1",
	}
	override := &api.ScionConfig{
		Image: "override-image:v1",
	}

	result := MergeScionConfig(base, override)
	if result.Image != "override-image:v1" {
		t.Errorf("MergeScionConfig: expected 'override-image:v1', got '%s'", result.Image)
	}

	// Test merge with empty override
	overrideEmpty := &api.ScionConfig{}
	resultEmpty := MergeScionConfig(base, overrideEmpty)
	if resultEmpty.Image != "base-image:v1" {
		t.Errorf("MergeScionConfig (empty override): expected 'base-image:v1', got '%s'", resultEmpty.Image)
	}
}

func TestMergeScionConfigServices(t *testing.T) {
	t.Run("override replaces base services", func(t *testing.T) {
		base := &api.ScionConfig{
			Services: []api.ServiceSpec{
				{Name: "svc1", Command: []string{"cmd1"}},
			},
		}
		override := &api.ScionConfig{
			Services: []api.ServiceSpec{
				{Name: "svc2", Command: []string{"cmd2"}},
				{Name: "svc3", Command: []string{"cmd3"}},
			},
		}
		result := MergeScionConfig(base, override)
		if len(result.Services) != 2 {
			t.Fatalf("expected 2 services, got %d", len(result.Services))
		}
		if result.Services[0].Name != "svc2" || result.Services[1].Name != "svc3" {
			t.Errorf("expected services [svc2, svc3], got [%s, %s]", result.Services[0].Name, result.Services[1].Name)
		}
	})

	t.Run("nil override preserves base services", func(t *testing.T) {
		base := &api.ScionConfig{
			Services: []api.ServiceSpec{
				{Name: "svc1", Command: []string{"cmd1"}},
			},
		}
		override := &api.ScionConfig{}
		result := MergeScionConfig(base, override)
		if len(result.Services) != 1 || result.Services[0].Name != "svc1" {
			t.Errorf("expected base services preserved, got %v", result.Services)
		}
	})

	t.Run("override with empty slice clears services", func(t *testing.T) {
		base := &api.ScionConfig{
			Services: []api.ServiceSpec{
				{Name: "svc1", Command: []string{"cmd1"}},
			},
		}
		override := &api.ScionConfig{
			Services: []api.ServiceSpec{},
		}
		result := MergeScionConfig(base, override)
		if len(result.Services) != 0 {
			t.Errorf("expected empty services, got %v", result.Services)
		}
	})

	t.Run("no base services with override", func(t *testing.T) {
		base := &api.ScionConfig{}
		override := &api.ScionConfig{
			Services: []api.ServiceSpec{
				{Name: "svc1", Command: []string{"cmd1"}},
			},
		}
		result := MergeScionConfig(base, override)
		if len(result.Services) != 1 || result.Services[0].Name != "svc1" {
			t.Errorf("expected override services, got %v", result.Services)
		}
	})
}

func TestValidateAgnosticTemplate_RejectsHarnessField(t *testing.T) {
	cfg := &api.ScionConfig{Harness: "claude"}
	err := ValidateAgnosticTemplate(cfg)
	if err == nil {
		t.Fatal("expected error when harness field is set, got nil")
	}
	if !strings.Contains(err.Error(), "'harness' field is no longer supported") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidateAgnosticTemplate_ValidTemplate(t *testing.T) {
	cfg := &api.ScionConfig{
		DefaultHarnessConfig: "gemini",
		AgentInstructions:    "agents.md",
		SystemPrompt:         "system-prompt.md",
	}
	err := ValidateAgnosticTemplate(cfg)
	if err != nil {
		t.Fatalf("expected no error for valid agnostic template, got: %v", err)
	}
}

func TestMergeScionConfig_NewFields(t *testing.T) {
	t.Run("agent_instructions override replaces base", func(t *testing.T) {
		base := &api.ScionConfig{AgentInstructions: "base-agents.md"}
		override := &api.ScionConfig{AgentInstructions: "override-agents.md"}
		got := MergeScionConfig(base, override)
		if got.AgentInstructions != "override-agents.md" {
			t.Errorf("expected AgentInstructions='override-agents.md', got %q", got.AgentInstructions)
		}
	})

	t.Run("agent_instructions empty override keeps base", func(t *testing.T) {
		base := &api.ScionConfig{AgentInstructions: "base-agents.md"}
		override := &api.ScionConfig{}
		got := MergeScionConfig(base, override)
		if got.AgentInstructions != "base-agents.md" {
			t.Errorf("expected AgentInstructions='base-agents.md', got %q", got.AgentInstructions)
		}
	})

	t.Run("system_prompt override replaces base", func(t *testing.T) {
		base := &api.ScionConfig{SystemPrompt: "base-prompt.md"}
		override := &api.ScionConfig{SystemPrompt: "override-prompt.md"}
		got := MergeScionConfig(base, override)
		if got.SystemPrompt != "override-prompt.md" {
			t.Errorf("expected SystemPrompt='override-prompt.md', got %q", got.SystemPrompt)
		}
	})

	t.Run("system_prompt empty override keeps base", func(t *testing.T) {
		base := &api.ScionConfig{SystemPrompt: "base-prompt.md"}
		override := &api.ScionConfig{}
		got := MergeScionConfig(base, override)
		if got.SystemPrompt != "base-prompt.md" {
			t.Errorf("expected SystemPrompt='base-prompt.md', got %q", got.SystemPrompt)
		}
	})

	t.Run("default_harness_config override replaces base", func(t *testing.T) {
		base := &api.ScionConfig{DefaultHarnessConfig: "gemini"}
		override := &api.ScionConfig{DefaultHarnessConfig: "claude"}
		got := MergeScionConfig(base, override)
		if got.DefaultHarnessConfig != "claude" {
			t.Errorf("expected DefaultHarnessConfig='claude', got %q", got.DefaultHarnessConfig)
		}
	})

	t.Run("default_harness_config empty override keeps base", func(t *testing.T) {
		base := &api.ScionConfig{DefaultHarnessConfig: "gemini"}
		override := &api.ScionConfig{}
		got := MergeScionConfig(base, override)
		if got.DefaultHarnessConfig != "gemini" {
			t.Errorf("expected DefaultHarnessConfig='gemini', got %q", got.DefaultHarnessConfig)
		}
	})

	t.Run("hub endpoint override replaces base", func(t *testing.T) {
		base := &api.ScionConfig{Hub: &api.AgentHubConfig{Endpoint: "https://base-hub.example.com"}}
		override := &api.ScionConfig{Hub: &api.AgentHubConfig{Endpoint: "https://override-hub.example.com"}}
		got := MergeScionConfig(base, override)
		if got.Hub == nil || got.Hub.Endpoint != "https://override-hub.example.com" {
			t.Errorf("expected Hub.Endpoint='https://override-hub.example.com', got %v", got.Hub)
		}
	})

	t.Run("hub nil override keeps base", func(t *testing.T) {
		base := &api.ScionConfig{Hub: &api.AgentHubConfig{Endpoint: "https://base-hub.example.com"}}
		override := &api.ScionConfig{}
		got := MergeScionConfig(base, override)
		if got.Hub == nil || got.Hub.Endpoint != "https://base-hub.example.com" {
			t.Errorf("expected Hub.Endpoint='https://base-hub.example.com', got %v", got.Hub)
		}
	})

	t.Run("hub override on nil base", func(t *testing.T) {
		base := &api.ScionConfig{}
		override := &api.ScionConfig{Hub: &api.AgentHubConfig{Endpoint: "https://new-hub.example.com"}}
		got := MergeScionConfig(base, override)
		if got.Hub == nil || got.Hub.Endpoint != "https://new-hub.example.com" {
			t.Errorf("expected Hub.Endpoint='https://new-hub.example.com', got %v", got.Hub)
		}
	})
}
