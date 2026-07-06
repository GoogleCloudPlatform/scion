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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runCmd(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("command %s %v in %s failed: %v\nOutput: %s", name, args, dir, err, string(output))
	}
}

func TestProvisionAgentHomeCopy(t *testing.T) {
	tmpDir := t.TempDir()

	// Move to tmpDir to avoid being inside the project's git repo
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	// Initialize dummy git repo
	runCmd(t, tmpDir, "git", "init")
	runCmd(t, tmpDir, "git", "config", "user.email", "test@example.com")
	runCmd(t, tmpDir, "git", "config", "user.name", "Test User")
	_ = os.WriteFile(filepath.Join(tmpDir, "initial"), []byte("initial"), 0644)
	runCmd(t, tmpDir, "git", "add", "initial")
	runCmd(t, tmpDir, "git", "commit", "-m", "initial commit")

	// Mock HOME for global settings and templates
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectScionDir := filepath.Join(tmpDir, ".scion")

	// Add .scion/agents/ to gitignore to satisfy ProvisionAgent's security check
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(".scion/agents/\n"), 0644)
	runCmd(t, tmpDir, "git", "add", ".gitignore")
	runCmd(t, tmpDir, "git", "commit", "-m", "add gitignore")

	// Create harness-config for test harness
	seedTestHarnessConfig(t, projectScionDir, "test", "test")

	// Create agnostic template with home directory
	_ = os.MkdirAll(filepath.Join(projectScionDir, "templates", "test-tpl", "home"), 0755)

	tplDir := filepath.Join(projectScionDir, "templates", "test-tpl")

	// Create file in template root (should NOT be copied)
	_ = os.WriteFile(filepath.Join(tplDir, "root-file.txt"), []byte("root"), 0644)

	// Create file in template home (SHOULD be copied as overlay)
	_ = os.WriteFile(filepath.Join(tplDir, "home", "home-file.txt"), []byte("home"), 0644)

	// Create agnostic scion-agent.json in template root
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test"}`), 0644)

	// Provision agent
	agentName := "test-agent"
	agentHome, _, _, err := ProvisionAgent(context.Background(), agentName, "test-tpl", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify home-file.txt exists in agentHome (from template overlay)
	if _, err := os.Stat(filepath.Join(agentHome, "home-file.txt")); os.IsNotExist(err) {
		t.Errorf("expected home-file.txt to be copied to agent home")
	}

	// Verify root-file.txt does NOT exist in agentHome
	if _, err := os.Stat(filepath.Join(agentHome, "root-file.txt")); err == nil {
		t.Errorf("expected root-file.txt NOT to be copied to agent home")
	}

	// Verify scion-agent.json does NOT exist in agentHome
	if _, err := os.Stat(filepath.Join(agentHome, "scion-agent.json")); err == nil {
		t.Errorf("expected scion-agent.json NOT to be copied to agent home")
	}
}

func TestProvisionAgentLegacyTemplateRejected(t *testing.T) {
	tmpDir := t.TempDir()

	// Move to tmpDir to avoid being inside the project's git repo
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	// Initialize dummy git repo
	runCmd(t, tmpDir, "git", "init")
	runCmd(t, tmpDir, "git", "config", "user.email", "test@example.com")
	runCmd(t, tmpDir, "git", "config", "user.name", "Test User")
	_ = os.WriteFile(filepath.Join(tmpDir, "initial"), []byte("initial"), 0644)
	runCmd(t, tmpDir, "git", "add", "initial")
	runCmd(t, tmpDir, "git", "commit", "-m", "initial commit")

	// Mock HOME
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectScionDir := filepath.Join(tmpDir, ".scion")

	// Add .scion/agents/ to gitignore
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(".scion/agents/\n"), 0644)
	runCmd(t, tmpDir, "git", "add", ".gitignore")
	runCmd(t, tmpDir, "git", "commit", "-m", "add gitignore")

	// Create a legacy template WITH a harness field (should be rejected)
	tplDir := filepath.Join(projectScionDir, "templates", "legacy-tpl")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"harness": "test"}`), 0644)

	// Provision agent - should fail with validation error
	agentName := "legacy-agent"
	_, _, _, err := ProvisionAgent(context.Background(), agentName, "legacy-tpl", "", "", projectScionDir, "", "", "", "")
	if err == nil {
		t.Fatal("expected error for legacy template with harness field, got nil")
	}

	if !strings.Contains(err.Error(), "harness") {
		t.Errorf("expected error to mention 'harness', got: %v", err)
	}
}
