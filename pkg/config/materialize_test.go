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

	"github.com/GoogleCloudPlatform/scion/resources"
)

func TestMaterializeBundledResources_SeedsAllResources(t *testing.T) {
	globalDir := t.TempDir()

	if err := MaterializeBundledResources(globalDir, MaterializeOptions{}); err != nil {
		t.Fatalf("MaterializeBundledResources failed: %v", err)
	}

	// Verify the default template was seeded.
	templateDir := filepath.Join(globalDir, "templates", "default")
	if _, err := os.Stat(filepath.Join(templateDir, "scion-agent.yaml")); err != nil {
		t.Errorf("expected scion-agent.yaml in default template: %v", err)
	}
	if _, err := os.Stat(filepath.Join(templateDir, "system-prompt.md")); err != nil {
		t.Errorf("expected system-prompt.md in default template: %v", err)
	}

	// Verify all bundled harness-configs were seeded.
	for _, res := range resources.BuiltinHarnessConfigs() {
		configPath := filepath.Join(globalDir, "harness-configs", res.Name, "config.yaml")
		if _, err := os.Stat(configPath); err != nil {
			t.Errorf("expected config.yaml for harness-config %q: %v", res.Name, err)
		}
	}
}

func TestMaterializeBundledResources_PreservesExistingWithoutForce(t *testing.T) {
	globalDir := t.TempDir()

	if err := MaterializeBundledResources(globalDir, MaterializeOptions{}); err != nil {
		t.Fatalf("initial MaterializeBundledResources failed: %v", err)
	}

	// Modify a template file.
	customContent := []byte("# custom user content")
	customFile := filepath.Join(globalDir, "templates", "default", "system-prompt.md")
	if err := os.WriteFile(customFile, customContent, 0644); err != nil {
		t.Fatalf("failed to write custom file: %v", err)
	}

	// Re-materialize without force.
	if err := MaterializeBundledResources(globalDir, MaterializeOptions{}); err != nil {
		t.Fatalf("second MaterializeBundledResources failed: %v", err)
	}

	// Verify the custom content was preserved.
	data, err := os.ReadFile(customFile)
	if err != nil {
		t.Fatalf("failed to read custom file: %v", err)
	}
	if string(data) != string(customContent) {
		t.Errorf("expected custom content to be preserved, got %q", string(data))
	}
}

func TestMaterializeBundledResources_ForceOverwrites(t *testing.T) {
	globalDir := t.TempDir()

	if err := MaterializeBundledResources(globalDir, MaterializeOptions{}); err != nil {
		t.Fatalf("initial MaterializeBundledResources failed: %v", err)
	}

	// Modify a template file.
	customContent := []byte("# custom user content")
	customFile := filepath.Join(globalDir, "templates", "default", "system-prompt.md")
	if err := os.WriteFile(customFile, customContent, 0644); err != nil {
		t.Fatalf("failed to write custom file: %v", err)
	}

	// Re-materialize with force.
	if err := MaterializeBundledResources(globalDir, MaterializeOptions{Force: true}); err != nil {
		t.Fatalf("force MaterializeBundledResources failed: %v", err)
	}

	// Verify the custom content was overwritten.
	data, err := os.ReadFile(customFile)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(data) == string(customContent) {
		t.Error("expected custom content to be overwritten with force=true")
	}
}

func TestMaterializeBundledResources_RestoresDeletedFiles(t *testing.T) {
	globalDir := t.TempDir()

	if err := MaterializeBundledResources(globalDir, MaterializeOptions{}); err != nil {
		t.Fatalf("initial MaterializeBundledResources failed: %v", err)
	}

	// Delete a built-in file.
	targetFile := filepath.Join(globalDir, "templates", "default", "system-prompt.md")
	if err := os.Remove(targetFile); err != nil {
		t.Fatalf("failed to remove file: %v", err)
	}

	// Re-materialize (without force — deleted files should still be restored).
	if err := MaterializeBundledResources(globalDir, MaterializeOptions{}); err != nil {
		t.Fatalf("second MaterializeBundledResources failed: %v", err)
	}

	if _, err := os.Stat(targetFile); err != nil {
		t.Errorf("expected deleted file to be restored: %v", err)
	}
}

func TestMaterializeSelectedHarnessConfigs_SeedsOnlyNamed(t *testing.T) {
	globalDir := t.TempDir()

	if err := MaterializeSelectedHarnessConfigs(globalDir, []string{"claude"}, MaterializeOptions{}); err != nil {
		t.Fatalf("MaterializeSelectedHarnessConfigs failed: %v", err)
	}

	// Claude should be seeded.
	claudeConfig := filepath.Join(globalDir, "harness-configs", "claude", "config.yaml")
	if _, err := os.Stat(claudeConfig); err != nil {
		t.Errorf("expected claude config.yaml: %v", err)
	}

	// Other harness-configs should NOT be seeded.
	allConfigs := resources.BuiltinHarnessConfigs()
	for _, res := range allConfigs {
		if res.Name == "claude" {
			continue
		}
		otherConfig := filepath.Join(globalDir, "harness-configs", res.Name, "config.yaml")
		if _, err := os.Stat(otherConfig); err == nil {
			t.Errorf("harness-config %q should not have been seeded in selective mode", res.Name)
		}
	}
}

func TestMaterializeSelectedHarnessConfigs_UnknownReturnsError(t *testing.T) {
	globalDir := t.TempDir()

	err := MaterializeSelectedHarnessConfigs(globalDir, []string{"nonexistent-harness"}, MaterializeOptions{})
	if err == nil {
		t.Fatal("expected error for unknown harness-config name")
	}
}

func TestMaterializeBundledTemplates_SeedsTemplatesOnly(t *testing.T) {
	globalDir := t.TempDir()

	if err := MaterializeBundledTemplates(globalDir, MaterializeOptions{}); err != nil {
		t.Fatalf("MaterializeBundledTemplates failed: %v", err)
	}

	// Template should be seeded.
	templateDir := filepath.Join(globalDir, "templates", "default")
	if _, err := os.Stat(filepath.Join(templateDir, "scion-agent.yaml")); err != nil {
		t.Errorf("expected scion-agent.yaml in default template: %v", err)
	}

	// No harness-configs should be seeded.
	harnessConfigsDir := filepath.Join(globalDir, "harness-configs")
	if _, err := os.Stat(harnessConfigsDir); err == nil {
		entries, _ := os.ReadDir(harnessConfigsDir)
		if len(entries) > 0 {
			t.Errorf("expected no harness-configs, got %d entries", len(entries))
		}
	}
}
