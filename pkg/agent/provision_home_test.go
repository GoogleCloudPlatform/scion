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
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Initialize dummy git repo
	runCmd(t, tmpDir, "git", "init")
	runCmd(t, tmpDir, "git", "config", "user.email", "test@example.com")
	runCmd(t, tmpDir, "git", "config", "user.name", "Test User")
	os.WriteFile(filepath.Join(tmpDir, "initial"), []byte("initial"), 0644)
	runCmd(t, tmpDir, "git", "add", "initial")
	runCmd(t, tmpDir, "git", "commit", "-m", "initial commit")

	// Mock HOME for global settings and templates
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	projectScionDir := filepath.Join(tmpDir, ".scion")
	
	// Add .scion/agents/ to gitignore to satisfy ProvisionAgent's security check
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(".scion/agents/\n"), 0644)
	runCmd(t, tmpDir, "git", "add", ".gitignore")
	runCmd(t, tmpDir, "git", "commit", "-m", "add gitignore")

	os.MkdirAll(filepath.Join(projectScionDir, "templates", "test-tpl", "home"), 0755)

	tplDir := filepath.Join(projectScionDir, "templates", "test-tpl")
	
	// Create file in template root (should NOT be copied if home exists)
	os.WriteFile(filepath.Join(tplDir, "root-file.txt"), []byte("root"), 0644)
	
	// Create file in template home (SHOULD be copied)
	os.WriteFile(filepath.Join(tplDir, "home", "home-file.txt"), []byte("home"), 0644)
	
	// Create scion-agent.json in template root
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"harness": "test"}`), 0644)

	// Provision agent
	agentName := "test-agent"
	agentHome, _, _, err := ProvisionAgent(context.Background(), agentName, "test-tpl", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify home-file.txt exists in agentHome
	if _, err := os.Stat(filepath.Join(agentHome, "home-file.txt")); os.IsNotExist(err) {
		t.Errorf("expected home-file.txt to be copied to agent home")
	}

	// Verify root-file.txt does NOT exist in agentHome
	if _, err := os.Stat(filepath.Join(agentHome, "root-file.txt")); err == nil {
		t.Errorf("expected root-file.txt NOT to be copied to agent home when home/ directory exists")
	}
	
	// Verify scion-agent.json does NOT exist in agentHome (it's in template root)
	if _, err := os.Stat(filepath.Join(agentHome, "scion-agent.json")); err == nil {
		t.Errorf("expected scion-agent.json NOT to be copied to agent home when home/ directory exists")
	}
}

func TestProvisionAgentLegacyTemplateCleanup(t *testing.T) {
	tmpDir := t.TempDir()

	// Move to tmpDir to avoid being inside the project's git repo
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Initialize dummy git repo
	runCmd(t, tmpDir, "git", "init")
	runCmd(t, tmpDir, "git", "config", "user.email", "test@example.com")
	runCmd(t, tmpDir, "git", "config", "user.name", "Test User")
	os.WriteFile(filepath.Join(tmpDir, "initial"), []byte("initial"), 0644)
	runCmd(t, tmpDir, "git", "add", "initial")
	runCmd(t, tmpDir, "git", "commit", "-m", "initial commit")

	// Mock HOME
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	projectScionDir := filepath.Join(tmpDir, ".scion")
	
	// Add .scion/agents/ to gitignore
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(".scion/agents/\n"), 0644)
	runCmd(t, tmpDir, "git", "add", ".gitignore")
	runCmd(t, tmpDir, "git", "commit", "-m", "add gitignore")

	// Create a template WITHOUT a home directory (legacy style)
	tplDir := filepath.Join(projectScionDir, "templates", "legacy-tpl")
	os.MkdirAll(tplDir, 0755)
	
	// Create scion-agent.json in template root
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"harness": "test"}`), 0644)
	
	// Create another file that SHOULD be copied
	os.WriteFile(filepath.Join(tplDir, "bashrc"), []byte("echo hello"), 0644)

	// Provision agent
	agentName := "legacy-agent"
	agentHome, _, _, err := ProvisionAgent(context.Background(), agentName, "legacy-tpl", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify bashrc exists
	if _, err := os.Stat(filepath.Join(agentHome, "bashrc")); os.IsNotExist(err) {
		t.Errorf("expected bashrc to be copied to agent home")
	}

	// Verify scion-agent.json should NOT exist in agentHome
	if _, err := os.Stat(filepath.Join(agentHome, "scion-agent.json")); err == nil {
		t.Errorf("scion-agent.json should NOT be present in agent home for legacy templates")
	}
}
