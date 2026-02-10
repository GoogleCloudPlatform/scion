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

package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ptone/scion-agent/pkg/api"
)

func TestCodexAuthPropagation(t *testing.T) {
	// Setup a temporary home directory
	tmpHome, err := os.MkdirTemp("", "scion-home-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpHome)

	// Mock HOME environment variable
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Create ~/.codex/auth.json
	codexDir := filepath.Join(tmpHome, ".codex")
	if err := os.MkdirAll(codexDir, 0755); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(codexDir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"token":"secret"}`), 0644); err != nil {
		t.Fatal(err)
	}

	c := &Codex{}
	agentHome := filepath.Join(tmpHome, "agent-home")
	
	// Discover Auth
	auth := c.DiscoverAuth(agentHome)
	if auth.CodexAuthFile != authPath {
		t.Errorf("expected CodexAuthFile to be %s, got %s", authPath, auth.CodexAuthFile)
	}

	// Propagate Files
	if err := c.PropagateFiles(agentHome, "user", auth); err != nil {
		t.Fatalf("PropagateFiles failed: %v", err)
	}

	// Verify file was copied
	dstPath := filepath.Join(agentHome, ".codex", "auth.json")
	if _, err := os.Stat(dstPath); err != nil {
		t.Errorf("expected auth file to be copied to %s", dstPath)
	}
	data, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"token":"secret"}` {
		t.Errorf("unexpected content in copied auth file")
	}
}

func TestCodexGetEnv(t *testing.T) {
	c := &Codex{}

	// Test OPENAI_API_KEY passthrough
	os.Setenv("OPENAI_API_KEY", "test-key")
	defer os.Unsetenv("OPENAI_API_KEY")

	env := c.GetEnv("test-agent", "/tmp", "user", api.AuthConfig{})
	if env["OPENAI_API_KEY"] != "test-key" {
		t.Errorf("expected OPENAI_API_KEY to be 'test-key', got '%s'", env["OPENAI_API_KEY"])
	}
}

func TestCodexGetCommand(t *testing.T) {
	c := &Codex{}

	// Test standard command
	cmd := c.GetCommand("do something", false, []string{})
	if len(cmd) < 3 || cmd[0] != "codex" || cmd[1] != "--yolo" || cmd[2] != "do something" {
		t.Errorf("unexpected command structure: %v", cmd)
	}

	// Test resume
	cmd = c.GetCommand("", true, []string{})
	if len(cmd) < 3 || cmd[1] != "--yolo" || cmd[2] != "resume" {
		t.Errorf("unexpected resume command: %v", cmd)
	}
}

func TestSeedTemplateDir_Codex(t *testing.T) {
	// Setup a temporary directory for the test
	tmpDir, err := os.MkdirTemp("", "scion-test-codex-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	templateDir := filepath.Join(tmpDir, "codex-template")
	c := &Codex{}
	
	err = c.SeedTemplateDir(templateDir, true)
	if err != nil {
		t.Fatalf("SeedTemplateDir failed: %v", err)
	}

	// Verify .codex directory exists
	codexDir := filepath.Join(templateDir, "home", ".codex")
	if info, err := os.Stat(codexDir); err != nil {
		t.Errorf("expected .codex directory to exist: %v", err)
	} else if !info.IsDir() {
		t.Errorf(".codex is not a directory")
	}

	// Verify config.toml exists
	configPath := filepath.Join(codexDir, "config.toml")
	if _, err := os.Stat(configPath); err != nil {
		t.Errorf("expected config.toml to exist: %v", err)
	}
}
