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
	"testing"

	"github.com/ptone/scion-agent/pkg/config"
)



func TestProvisionAgentReloadsConfig(t *testing.T) {
	// This test verifies that ProvisionAgent reloads the config after harness.Provision
	// which allows harness-injected changes (like GEMINI_API_KEY) to be returned.

	tmpDir := t.TempDir()

	// Move to tmpDir
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Mock HOME
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

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

	// Provision a gemini agent
	// The default "gemini" template uses "gemini" harness and default "gemini-api-key" auth type.
	// The GeminiCLI harness injects GEMINI_API_KEY=${GEMINI_API_KEY} into scion-agent.json during Provision.
	agentName := "reload-test-agent"
	_, _, cfg, err := ProvisionAgent(context.Background(), agentName, "gemini", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// Verify env
	if cfg.Env == nil {
		t.Fatal("expected cfg.Env to be non-nil")
	}

	val, ok := cfg.Env["GEMINI_API_KEY"]
	if !ok {
		t.Error("expected GEMINI_API_KEY to be in returned config Env")
	}
	if val != "${GEMINI_API_KEY}" {
		t.Errorf("expected GEMINI_API_KEY to be '${GEMINI_API_KEY}', got '%s'", val)
	}
}
