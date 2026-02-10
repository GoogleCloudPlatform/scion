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
	mockGemini := &MockHarness{NameVal: "gemini", EmbedDirVal: "gemini", ConfigDirVal: ".gemini"}
	err = CreateTemplate(tplName, mockGemini, false)
	if err != nil {
		t.Fatalf("failed to create project template: %v", err)
	}

	expectedPath := filepath.Join(projectDir, "templates", tplName)
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Errorf("expected template directory %s to exist", expectedPath)
	}

	// Verify key files exist (scion-agent is now YAML)
	files := []string{
		"scion-agent.yaml",
		filepath.Join("home", ".bashrc"),
		filepath.Join("home", ".gemini", "settings.json"),
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(expectedPath, f)); os.IsNotExist(err) {
			t.Errorf("expected file %s to exist in template", f)
		}
	}

	// Test creating a global template
	globalTplName := "global-tpl"
	err = CreateTemplate(globalTplName, mockGemini, true)
	if err != nil {
		t.Fatalf("failed to create global template: %v", err)
	}

	globalExpectedPath := filepath.Join(tmpDir, GlobalDir, "templates", globalTplName)
	if _, err := os.Stat(globalExpectedPath); os.IsNotExist(err) {
		t.Errorf("expected global template directory %s to exist", globalExpectedPath)
	}

	// Test duplicate template creation fails
	err = CreateTemplate(tplName, mockGemini, false)
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
	mockGemini := &MockHarness{NameVal: "gemini", EmbedDirVal: "gemini", ConfigDirVal: ".gemini"}
	if err := CreateTemplate(tplName, mockGemini, false); err != nil {
		t.Fatal(err)
	}
	globalTplName := "global-tpl-delete"
	if err := CreateTemplate(globalTplName, mockGemini, true); err != nil {
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

	// Initialize project (creates default templates)
	if err := InitProject("", GetMockHarnesses()); err != nil {
		t.Fatal(err)
	}

	geminiDefaultScionYAML := filepath.Join(projectDir, "templates", "gemini", "scion-agent.yaml")

	// Corrupt the default template file
	corruptContent := "CORRUPT"
	if err := os.WriteFile(geminiDefaultScionYAML, []byte(corruptContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Update default templates
	if err := UpdateDefaultTemplates(false, GetMockHarnesses()); err != nil {
		t.Fatalf("failed to update default templates: %v", err)
	}

	// Verify it was restored
	data, err := os.ReadFile(geminiDefaultScionYAML)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == corruptContent {
		t.Error("expected scion-agent.yaml to be overwritten, but it still contains corrupt content")
	}

	// Verify settings.json content
	geminiSettingsPath := filepath.Join(projectDir, "templates", "gemini", "home", ".gemini", "settings.json")
	settingsData, err := os.ReadFile(geminiSettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(settingsData, &settings); err != nil {
		t.Fatalf("failed to unmarshal settings.json: %v", err)
	}

	security, ok := settings["security"].(map[string]interface{})
	if !ok {
		t.Fatal("security section missing in settings.json")
	}
	auth, ok := security["auth"].(map[string]interface{})
	if !ok {
		t.Fatal("auth section missing in security section")
	}
	if auth["selectedType"] != "gemini-api-key" {
		t.Errorf("expected selectedType to be gemini-api-key, got %v", auth["selectedType"])
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
	mockGemini := &MockHarness{NameVal: "gemini", EmbedDirVal: "gemini", ConfigDirVal: ".gemini"}
	if err := CreateTemplate(srcName, mockGemini, false); err != nil {
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

	// Verify key files exist in destination (scion-agent is now YAML)
	files := []string{
		"scion-agent.yaml",
		filepath.Join("home", ".bashrc"),
		filepath.Join("home", ".gemini", "settings.json"),
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
