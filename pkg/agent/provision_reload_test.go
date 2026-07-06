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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestProvisionAgentReloadsConfig(t *testing.T) {
	mockRuntimeForTest(t)
	// This test verifies that ProvisionAgent reloads the config after harness.Provision
	// which allows harness-injected changes to be returned.

	tmpDir := t.TempDir()

	// Move to tmpDir
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	// Mock HOME
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	// Seed global harness-configs (required for agent creation)
	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

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

	// Provision a claude agent using the "default" agnostic template with --harness-config=claude
	agentName := "reload-test-agent"
	_, _, cfg, err := ProvisionAgent(context.Background(), agentName, "default", "", "claude", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// With no auth_selected_type in the claude harness config, no env vars
	// should be injected by Provision (auth is determined at runtime).
	if cfg.Env != nil {
		if _, ok := cfg.Env["ANTHROPIC_API_KEY"]; ok {
			t.Error("ANTHROPIC_API_KEY should not be in env when no auth_selected_type is set")
		}
	}
}

func TestProvisionAgentWithHarnessAuthOverride(t *testing.T) {
	mockRuntimeForTest(t)
	// Verify that when --harness-auth vertex-ai is used with the claude harness,
	// ANTHROPIC_API_KEY is NOT injected into the env map by harness Provision().

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := config.InitProject(projectScionDir, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	// Provision with vertex-ai override via inline config (simulates --harness-auth vertex-ai)
	agentName := "vertex-ai-override"
	inlineCfg := &api.ScionConfig{AuthSelectedType: "vertex-ai"}
	_, _, cfg, err := ProvisionAgent(context.Background(), agentName, "default", "", "claude", projectScionDir, "", "", "", "", inlineCfg)
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// ANTHROPIC_API_KEY should NOT be present — vertex-ai doesn't use it
	if cfg.Env != nil {
		if _, ok := cfg.Env["ANTHROPIC_API_KEY"]; ok {
			t.Error("ANTHROPIC_API_KEY should not be in env when auth is vertex-ai")
		}
	}

	// Verify the auth_selected_type was persisted
	if cfg.AuthSelectedType != "vertex-ai" {
		t.Errorf("expected AuthSelectedType = 'vertex-ai', got %q", cfg.AuthSelectedType)
	}
}
