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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

	// Override home dir so global dir resolves to tmpDir
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	globalDefaultDir := filepath.Join(tmpDir, DotScion, "templates", "default")
	defaultScionYAML := filepath.Join(globalDefaultDir, "scion-agent.yaml")

	// First call: no existing default template, should succeed without force
	if err := UpdateDefaultTemplates(false, GetMockHarnesses()); err != nil {
		t.Fatalf("expected first update to succeed, got: %v", err)
	}

	// Verify the default agnostic template was created
	data, err := os.ReadFile(defaultScionYAML)
	if err != nil {
		t.Fatalf("expected scion-agent.yaml to exist after update: %v", err)
	}
	originalContent := string(data)
	if originalContent == "" {
		t.Fatal("expected scion-agent.yaml to have content")
	}

	// Second call without force: should fail because default already exists
	err = UpdateDefaultTemplates(false, GetMockHarnesses())
	if err == nil {
		t.Fatal("expected error when updating existing default without force, got nil")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("expected error to mention --force, got: %v", err)
	}

	// Corrupt the file to verify force actually overwrites
	corruptContent := "CORRUPT"
	if err := os.WriteFile(defaultScionYAML, []byte(corruptContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Third call with force: should succeed and overwrite
	if err := UpdateDefaultTemplates(true, GetMockHarnesses()); err != nil {
		t.Fatalf("expected force update to succeed, got: %v", err)
	}

	// Verify the default agnostic template was restored
	data, err = os.ReadFile(defaultScionYAML)
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
		name      string
		base      *api.ScionConfig
		override  *api.ScionConfig
		wantPhase string
	}{
		{
			name:      "override phase",
			base:      &api.ScionConfig{Info: &api.AgentInfo{Phase: "created"}},
			override:  &api.ScionConfig{Info: &api.AgentInfo{Phase: "running"}},
			wantPhase: "running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeScionConfig(tt.base, tt.override)
			if got.Info == nil || got.Info.Phase != tt.wantPhase {
				t.Errorf("MergeScionConfig() Phase = %v, want %v", got.Info.Phase, tt.wantPhase)
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

func TestFindTemplateInGrovePath_GitGroveInRepoTemplates(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Simulate a git grove: in-repo .scion/ with grove-id and templates/ in-repo.
	// Templates live in-repo so they can be committed to the repository.
	projectDir := filepath.Join(t.TempDir(), "my-git-project", ".scion")
	os.MkdirAll(projectDir, 0755)
	if err := WriteGroveID(projectDir, "550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Fatal(err)
	}

	// Templates are stored in the in-repo .scion/templates/ directory
	inRepoTplDir := filepath.Join(projectDir, "templates", "my-tpl")
	if err := os.MkdirAll(inRepoTplDir, 0755); err != nil {
		t.Fatal(err)
	}

	tpl, err := FindTemplateInGrovePath("my-tpl", projectDir)
	if err != nil {
		t.Fatalf("FindTemplateInGrovePath failed: %v", err)
	}
	if tpl.Path != inRepoTplDir {
		t.Errorf("expected template path %q, got %q", inRepoTplDir, tpl.Path)
	}
	if tpl.Scope != "grove" {
		t.Errorf("expected scope 'grove', got %q", tpl.Scope)
	}
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

func TestGetTemplateChainInGroveWithDefault(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "scion-test-chain-default-*")
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

	grovePath := filepath.Join(tmpDir, "project", DotScion)

	// Create both default and custom templates in the grove
	defaultTplDir := filepath.Join(grovePath, "templates", "default")
	customTplDir := filepath.Join(grovePath, "templates", "custom")
	if err := os.MkdirAll(defaultTplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(customTplDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Non-default template should produce a 2-link chain: [default, custom]
	chain, err := GetTemplateChainInGrove("custom", grovePath)
	if err != nil {
		t.Fatalf("GetTemplateChainInGrove failed: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("expected chain length 2, got %d", len(chain))
	}
	if chain[0].Path != defaultTplDir {
		t.Errorf("expected chain[0] path %q, got %q", defaultTplDir, chain[0].Path)
	}
	if chain[1].Path != customTplDir {
		t.Errorf("expected chain[1] path %q, got %q", customTplDir, chain[1].Path)
	}

	// Default template itself should produce a 1-link chain: [default]
	chain, err = GetTemplateChainInGrove("default", grovePath)
	if err != nil {
		t.Fatalf("GetTemplateChainInGrove for default failed: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("expected chain length 1 for default, got %d", len(chain))
	}
	if chain[0].Path != defaultTplDir {
		t.Errorf("expected path %q, got %q", defaultTplDir, chain[0].Path)
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

func TestMergeScionConfig_TaskFlag(t *testing.T) {
	t.Run("task_flag override replaces base", func(t *testing.T) {
		base := &api.ScionConfig{TaskFlag: "--task"}
		override := &api.ScionConfig{TaskFlag: "--input"}
		got := MergeScionConfig(base, override)
		if got.TaskFlag != "--input" {
			t.Errorf("expected TaskFlag='--input', got %q", got.TaskFlag)
		}
	})

	t.Run("task_flag empty override keeps base", func(t *testing.T) {
		base := &api.ScionConfig{TaskFlag: "--input"}
		override := &api.ScionConfig{}
		got := MergeScionConfig(base, override)
		if got.TaskFlag != "--input" {
			t.Errorf("expected TaskFlag='--input', got %q", got.TaskFlag)
		}
	})

	t.Run("task_flag set on nil base", func(t *testing.T) {
		got := MergeScionConfig(nil, &api.ScionConfig{TaskFlag: "--input"})
		if got.TaskFlag != "--input" {
			t.Errorf("expected TaskFlag='--input', got %q", got.TaskFlag)
		}
	})
}

func TestMergeScionConfig_InlineConfigFields(t *testing.T) {
	t.Run("user override replaces base", func(t *testing.T) {
		base := &api.ScionConfig{User: "root"}
		override := &api.ScionConfig{User: "scion"}
		got := MergeScionConfig(base, override)
		if got.User != "scion" {
			t.Errorf("expected User='scion', got %q", got.User)
		}
	})

	t.Run("user empty override keeps base", func(t *testing.T) {
		base := &api.ScionConfig{User: "scion"}
		override := &api.ScionConfig{}
		got := MergeScionConfig(base, override)
		if got.User != "scion" {
			t.Errorf("expected User='scion', got %q", got.User)
		}
	})

	t.Run("task override replaces base", func(t *testing.T) {
		base := &api.ScionConfig{Task: "old task"}
		override := &api.ScionConfig{Task: "new task"}
		got := MergeScionConfig(base, override)
		if got.Task != "new task" {
			t.Errorf("expected Task='new task', got %q", got.Task)
		}
	})

	t.Run("task empty override keeps base", func(t *testing.T) {
		base := &api.ScionConfig{Task: "existing task"}
		override := &api.ScionConfig{}
		got := MergeScionConfig(base, override)
		if got.Task != "existing task" {
			t.Errorf("expected Task='existing task', got %q", got.Task)
		}
	})

	t.Run("branch override replaces base", func(t *testing.T) {
		base := &api.ScionConfig{Branch: "main"}
		override := &api.ScionConfig{Branch: "feature-branch"}
		got := MergeScionConfig(base, override)
		if got.Branch != "feature-branch" {
			t.Errorf("expected Branch='feature-branch', got %q", got.Branch)
		}
	})

	t.Run("branch empty override keeps base", func(t *testing.T) {
		base := &api.ScionConfig{Branch: "develop"}
		override := &api.ScionConfig{}
		got := MergeScionConfig(base, override)
		if got.Branch != "develop" {
			t.Errorf("expected Branch='develop', got %q", got.Branch)
		}
	})

	t.Run("max_model_calls override replaces base", func(t *testing.T) {
		base := &api.ScionConfig{MaxModelCalls: 100}
		override := &api.ScionConfig{MaxModelCalls: 200}
		got := MergeScionConfig(base, override)
		if got.MaxModelCalls != 200 {
			t.Errorf("expected MaxModelCalls=200, got %d", got.MaxModelCalls)
		}
	})

	t.Run("max_model_calls zero override keeps base", func(t *testing.T) {
		base := &api.ScionConfig{MaxModelCalls: 150}
		override := &api.ScionConfig{}
		got := MergeScionConfig(base, override)
		if got.MaxModelCalls != 150 {
			t.Errorf("expected MaxModelCalls=150, got %d", got.MaxModelCalls)
		}
	})

	t.Run("full inline config merge over template", func(t *testing.T) {
		template := &api.ScionConfig{
			Model:         "claude-sonnet-4-6",
			MaxTurns:      100,
			HarnessConfig: "claude-default",
			User:          "root",
		}
		inline := &api.ScionConfig{
			Model:  "claude-opus-4-6",
			Task:   "Review the code",
			Branch: "review-branch",
			User:   "scion",
		}
		got := MergeScionConfig(template, inline)
		if got.Model != "claude-opus-4-6" {
			t.Errorf("expected Model='claude-opus-4-6', got %q", got.Model)
		}
		if got.MaxTurns != 100 {
			t.Errorf("expected MaxTurns=100 (from template), got %d", got.MaxTurns)
		}
		if got.HarnessConfig != "claude-default" {
			t.Errorf("expected HarnessConfig='claude-default' (from template), got %q", got.HarnessConfig)
		}
		if got.Task != "Review the code" {
			t.Errorf("expected Task='Review the code', got %q", got.Task)
		}
		if got.Branch != "review-branch" {
			t.Errorf("expected Branch='review-branch', got %q", got.Branch)
		}
		if got.User != "scion" {
			t.Errorf("expected User='scion', got %q", got.User)
		}
	})
}

func boolP(b bool) *bool          { return &b }
func float64P(f float64) *float64 { return &f }

func TestMergeScionConfigTelemetry(t *testing.T) {
	t.Run("override on nil base", func(t *testing.T) {
		base := &api.ScionConfig{}
		override := &api.ScionConfig{
			Telemetry: &api.TelemetryConfig{
				Enabled: boolP(false),
				Cloud: &api.TelemetryCloudConfig{
					Endpoint: "https://otel.example.com",
				},
			},
		}
		got := MergeScionConfig(base, override)
		if got.Telemetry == nil {
			t.Fatal("expected Telemetry to be set")
		}
		if got.Telemetry.Enabled == nil || *got.Telemetry.Enabled != false {
			t.Errorf("expected Telemetry.Enabled=false, got %v", got.Telemetry.Enabled)
		}
		if got.Telemetry.Cloud == nil || got.Telemetry.Cloud.Endpoint != "https://otel.example.com" {
			t.Errorf("expected Cloud.Endpoint, got %v", got.Telemetry.Cloud)
		}
	})

	t.Run("nil override preserves base", func(t *testing.T) {
		base := &api.ScionConfig{
			Telemetry: &api.TelemetryConfig{
				Enabled: boolP(true),
				Cloud: &api.TelemetryCloudConfig{
					Endpoint: "https://base.example.com",
					Protocol: "grpc",
				},
			},
		}
		override := &api.ScionConfig{}
		got := MergeScionConfig(base, override)
		if got.Telemetry == nil {
			t.Fatal("expected Telemetry to be preserved")
		}
		if got.Telemetry.Cloud == nil || got.Telemetry.Cloud.Endpoint != "https://base.example.com" {
			t.Errorf("expected base endpoint preserved")
		}
	})

	t.Run("partial override merges fields", func(t *testing.T) {
		base := &api.ScionConfig{
			Telemetry: &api.TelemetryConfig{
				Enabled: boolP(true),
				Cloud: &api.TelemetryCloudConfig{
					Endpoint: "https://base.example.com",
					Protocol: "grpc",
					TLS: &api.TelemetryTLS{
						Enabled:            boolP(true),
						InsecureSkipVerify: boolP(false),
						CAFile:             "/etc/ssl/certs/base-root.pem",
						CertFile:           "/etc/ssl/certs/base-client.pem",
						KeyFile:            "/etc/ssl/private/base-client-key.pem",
					},
					Batch: &api.TelemetryBatch{
						MaxSize: 512,
						Timeout: "5s",
					},
				},
				Hub: &api.TelemetryHubConfig{
					Enabled:        boolP(true),
					ReportInterval: "30s",
				},
				Filter: &api.TelemetryFilterConfig{
					Events: &api.TelemetryEventsConfig{
						Exclude: []string{"agent.user.prompt"},
					},
					Attributes: &api.TelemetryAttributesConfig{
						Redact: []string{"prompt"},
						Hash:   []string{"session_id"},
					},
					Sampling: &api.TelemetrySamplingConfig{
						Default: float64P(1.0),
						Rates:   map[string]float64{"agent.tool.call": 0.5},
					},
				},
				Resource: map[string]string{
					"service.name": "base-agent",
				},
			},
		}
		override := &api.ScionConfig{
			Telemetry: &api.TelemetryConfig{
				Cloud: &api.TelemetryCloudConfig{
					Endpoint: "https://override.example.com",
					TLS: &api.TelemetryTLS{
						InsecureSkipVerify: boolP(true),
						CAFile:             "/etc/ssl/certs/override-root.pem",
						CertFile:           "/etc/ssl/certs/override-client.pem",
						KeyFile:            "/etc/ssl/private/override-client-key.pem",
					},
					Batch: &api.TelemetryBatch{
						MaxSize: 256,
					},
				},
				Hub: &api.TelemetryHubConfig{
					Enabled: boolP(false),
				},
				Filter: &api.TelemetryFilterConfig{
					Events: &api.TelemetryEventsConfig{
						Exclude: []string{"agent.tool.output"},
					},
					Sampling: &api.TelemetrySamplingConfig{
						Default: float64P(0.5),
						Rates:   map[string]float64{"agent.cost": 0.1},
					},
				},
				Resource: map[string]string{
					"deployment.env": "production",
				},
			},
		}

		got := MergeScionConfig(base, override)
		if got.Telemetry == nil {
			t.Fatal("expected Telemetry")
		}

		// Enabled should come from base (not overridden)
		if got.Telemetry.Enabled == nil || *got.Telemetry.Enabled != true {
			t.Errorf("expected Enabled=true from base")
		}

		// Cloud endpoint overridden
		if got.Telemetry.Cloud.Endpoint != "https://override.example.com" {
			t.Errorf("expected overridden endpoint, got %s", got.Telemetry.Cloud.Endpoint)
		}
		// Cloud protocol preserved from base
		if got.Telemetry.Cloud.Protocol != "grpc" {
			t.Errorf("expected protocol preserved, got %s", got.Telemetry.Cloud.Protocol)
		}
		// TLS enabled preserved, insecure overridden
		if got.Telemetry.Cloud.TLS.Enabled == nil || *got.Telemetry.Cloud.TLS.Enabled != true {
			t.Errorf("expected TLS.Enabled=true preserved")
		}
		if got.Telemetry.Cloud.TLS.InsecureSkipVerify == nil || *got.Telemetry.Cloud.TLS.InsecureSkipVerify != true {
			t.Errorf("expected InsecureSkipVerify=true overridden")
		}
		if got.Telemetry.Cloud.TLS.CAFile != "/etc/ssl/certs/override-root.pem" {
			t.Errorf("expected CAFile override preserved, got %q", got.Telemetry.Cloud.TLS.CAFile)
		}
		// Batch max_size overridden, timeout preserved
		if got.Telemetry.Cloud.Batch.MaxSize != 256 {
			t.Errorf("expected Batch.MaxSize=256, got %d", got.Telemetry.Cloud.Batch.MaxSize)
		}
		if got.Telemetry.Cloud.Batch.Timeout != "5s" {
			t.Errorf("expected Batch.Timeout='5s' preserved, got %s", got.Telemetry.Cloud.Batch.Timeout)
		}

		// Hub enabled overridden, report_interval preserved
		if got.Telemetry.Hub.Enabled == nil || *got.Telemetry.Hub.Enabled != false {
			t.Errorf("expected Hub.Enabled=false overridden")
		}
		if got.Telemetry.Hub.ReportInterval != "30s" {
			t.Errorf("expected Hub.ReportInterval='30s' preserved")
		}

		// Filter events overridden (last write wins for arrays)
		if len(got.Telemetry.Filter.Events.Exclude) != 1 || got.Telemetry.Filter.Events.Exclude[0] != "agent.tool.output" {
			t.Errorf("expected events.exclude overridden to [agent.tool.output], got %v", got.Telemetry.Filter.Events.Exclude)
		}
		// Filter attributes preserved (not overridden)
		if len(got.Telemetry.Filter.Attributes.Redact) != 1 || got.Telemetry.Filter.Attributes.Redact[0] != "prompt" {
			t.Errorf("expected attributes.redact preserved")
		}
		// Sampling default overridden
		if got.Telemetry.Filter.Sampling.Default == nil || *got.Telemetry.Filter.Sampling.Default != 0.5 {
			t.Errorf("expected sampling.default=0.5")
		}
		// Sampling rates merged (both keys present)
		if got.Telemetry.Filter.Sampling.Rates["agent.tool.call"] != 0.5 {
			t.Errorf("expected agent.tool.call rate preserved")
		}
		if got.Telemetry.Filter.Sampling.Rates["agent.cost"] != 0.1 {
			t.Errorf("expected agent.cost rate added")
		}

		// Resource merged
		if got.Telemetry.Resource["service.name"] != "base-agent" {
			t.Errorf("expected service.name preserved")
		}
		if got.Telemetry.Resource["deployment.env"] != "production" {
			t.Errorf("expected deployment.env added")
		}
	})
}

func TestLoadConfigYAMLKeyNormalization(t *testing.T) {
	t.Run("harness-config hyphen maps to harness_config", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "scion-test-yaml-normalize-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(tmpDir)

		configContent := `harness-config: claude-web
`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.yaml"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}

		tpl := &Template{Path: tmpDir}
		cfg, err := tpl.LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig failed: %v", err)
		}

		if cfg.HarnessConfig != "claude-web" {
			t.Errorf("expected HarnessConfig='claude-web', got %q", cfg.HarnessConfig)
		}
	})

	t.Run("default-harness-config hyphen maps to default_harness_config", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "scion-test-yaml-normalize-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(tmpDir)

		configContent := `default-harness-config: gemini-experimental
`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.yaml"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}

		tpl := &Template{Path: tmpDir}
		cfg, err := tpl.LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig failed: %v", err)
		}

		if cfg.DefaultHarnessConfig != "gemini-experimental" {
			t.Errorf("expected DefaultHarnessConfig='gemini-experimental', got %q", cfg.DefaultHarnessConfig)
		}
	})

	t.Run("underscore keys still work", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "scion-test-yaml-normalize-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(tmpDir)

		configContent := `harness_config: claude-web
default_harness_config: gemini
command_args:
  - "--verbose"
`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.yaml"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}

		tpl := &Template{Path: tmpDir}
		cfg, err := tpl.LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig failed: %v", err)
		}

		if cfg.HarnessConfig != "claude-web" {
			t.Errorf("expected HarnessConfig='claude-web', got %q", cfg.HarnessConfig)
		}
		if cfg.DefaultHarnessConfig != "gemini" {
			t.Errorf("expected DefaultHarnessConfig='gemini', got %q", cfg.DefaultHarnessConfig)
		}
		if len(cfg.CommandArgs) != 1 || cfg.CommandArgs[0] != "--verbose" {
			t.Errorf("expected CommandArgs=['--verbose'], got %v", cfg.CommandArgs)
		}
	})

	t.Run("env map keys are not normalized", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "scion-test-yaml-normalize-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(tmpDir)

		configContent := `env:
  MY-CUSTOM-VAR: hello
  NORMAL_VAR: world
`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.yaml"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}

		tpl := &Template{Path: tmpDir}
		cfg, err := tpl.LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig failed: %v", err)
		}

		// Top-level key "env" has no hyphens, so it stays as-is.
		// The nested map keys should NOT be normalized since normalizeYAMLMappingKeys
		// only processes the top level.
		if cfg.Env == nil {
			t.Fatal("expected Env to be non-nil")
		}
		if _, ok := cfg.Env["NORMAL_VAR"]; !ok {
			t.Error("expected NORMAL_VAR in env")
		}
	})

	t.Run("full template with mixed hyphen keys", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "scion-test-yaml-normalize-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(tmpDir)

		configContent := `harness-config: claude-web
image: us-central1-docker.pkg.dev/my-project/repo/image:latest
services:
  - name: chromium
    command:
      - "chromium"
      - "--headless"
`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.yaml"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}

		tpl := &Template{Path: tmpDir}
		cfg, err := tpl.LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig failed: %v", err)
		}

		if cfg.HarnessConfig != "claude-web" {
			t.Errorf("expected HarnessConfig='claude-web', got %q", cfg.HarnessConfig)
		}
		// Image value should NOT be modified (hyphens in values are fine)
		if cfg.Image != "us-central1-docker.pkg.dev/my-project/repo/image:latest" {
			t.Errorf("expected Image value preserved with hyphens, got %q", cfg.Image)
		}
		if len(cfg.Services) != 1 || cfg.Services[0].Name != "chromium" {
			t.Errorf("expected 1 service named 'chromium', got %v", cfg.Services)
		}
	})
}

func TestNormalizeYAMLMappingKeys(t *testing.T) {
	t.Run("normalizes hyphens to underscores in mapping keys", func(t *testing.T) {
		input := `harness-config: claude-web
default-harness-config: gemini
command-args:
  - "--verbose"
`
		var cfg api.ScionConfig
		if err := unmarshalYAMLNormalized([]byte(input), &cfg); err != nil {
			t.Fatalf("unmarshalYAMLNormalized failed: %v", err)
		}

		if cfg.HarnessConfig != "claude-web" {
			t.Errorf("expected HarnessConfig='claude-web', got %q", cfg.HarnessConfig)
		}
		if cfg.DefaultHarnessConfig != "gemini" {
			t.Errorf("expected DefaultHarnessConfig='gemini', got %q", cfg.DefaultHarnessConfig)
		}
		if len(cfg.CommandArgs) != 1 || cfg.CommandArgs[0] != "--verbose" {
			t.Errorf("expected CommandArgs=['--verbose'], got %v", cfg.CommandArgs)
		}
	})

	t.Run("does not modify values", func(t *testing.T) {
		input := `image: my-registry/my-image:latest
model: gemini-pro
`
		var cfg api.ScionConfig
		if err := unmarshalYAMLNormalized([]byte(input), &cfg); err != nil {
			t.Fatalf("unmarshalYAMLNormalized failed: %v", err)
		}

		if cfg.Image != "my-registry/my-image:latest" {
			t.Errorf("expected Image value preserved, got %q", cfg.Image)
		}
		if cfg.Model != "gemini-pro" {
			t.Errorf("expected Model value preserved, got %q", cfg.Model)
		}
	})
}

func TestValidateAgentConfig_Telemetry(t *testing.T) {
	data := []byte(`
schema_version: "1"
telemetry:
  enabled: false
  cloud:
    endpoint: "https://agent-otel.example.com"
  filter:
    events:
      exclude:
        - "agent.user.prompt"
`)
	errors, err := ValidateAgentConfig(data, "1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errors) > 0 {
		t.Errorf("expected no validation errors, got: %v", errors)
	}
}

func TestFriendlyTemplateName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty", "", ""},
		{"simple name", "claude", "claude"},
		{"simple name with dash", "my-template", "my-template"},
		{"absolute path", "/home/user/.scion/templates/cache/abc123", "abc123"},
		{"cache path", "/tmp/.scion_cache/templates/my-template", "my-template"},
		{"http URI", "https://example.com/my-template.tar.gz", "my-template"},
		{"github URI", "https://github.com/user/repo/tree/main/templates/claude", "claude"},
		{"rclone path", ":gcs:bucket/path/to/template", "template"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FriendlyTemplateName(tt.input)
			if result != tt.expected {
				t.Errorf("FriendlyTemplateName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestResolveContentInChain(t *testing.T) {
	t.Run("file in parent template is found when missing from child", func(t *testing.T) {
		parentDir := t.TempDir()
		childDir := t.TempDir()

		expectedContent := "# Base agent instructions\nBe helpful."
		os.WriteFile(filepath.Join(parentDir, "agents.md"), []byte(expectedContent), 0644)

		chain := []*Template{
			{Name: "default", Path: parentDir},
			{Name: "custom", Path: childDir},
		}

		content, err := ResolveContentInChain(chain, "agents.md")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(content) != expectedContent {
			t.Errorf("expected content from parent template, got %q", string(content))
		}
	})

	t.Run("file in child template takes precedence over parent", func(t *testing.T) {
		parentDir := t.TempDir()
		childDir := t.TempDir()

		os.WriteFile(filepath.Join(parentDir, "agents.md"), []byte("parent content"), 0644)
		os.WriteFile(filepath.Join(childDir, "agents.md"), []byte("child content"), 0644)

		chain := []*Template{
			{Name: "default", Path: parentDir},
			{Name: "custom", Path: childDir},
		}

		content, err := ResolveContentInChain(chain, "agents.md")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(content) != "child content" {
			t.Errorf("expected child content to take precedence, got %q", string(content))
		}
	})

	t.Run("inline content returned when no template has the file", func(t *testing.T) {
		dir1 := t.TempDir()
		dir2 := t.TempDir()

		chain := []*Template{
			{Name: "default", Path: dir1},
			{Name: "custom", Path: dir2},
		}

		content, err := ResolveContentInChain(chain, "You are a helpful assistant.")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(content) != "You are a helpful assistant." {
			t.Errorf("expected inline content, got %q", string(content))
		}
	})

	t.Run("empty field returns nil", func(t *testing.T) {
		chain := []*Template{{Name: "default", Path: t.TempDir()}}
		content, err := ResolveContentInChain(chain, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if content != nil {
			t.Errorf("expected nil for empty field, got %q", string(content))
		}
	})
}
