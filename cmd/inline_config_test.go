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

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestLoadInlineConfig_YAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	yamlContent := `
harness_config: claude-default
model: claude-opus-4-6
image: my-image:latest
user: scion
task: "Review the code"
branch: review-branch
max_turns: 50
max_duration: 30m
system_prompt: |
  You are a code reviewer.
env:
  LOG_LEVEL: debug
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, configDir, err := loadInlineConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if configDir != dir {
		t.Errorf("expected configDir=%q, got %q", dir, configDir)
	}
	if cfg.HarnessConfig != "claude-default" {
		t.Errorf("expected HarnessConfig='claude-default', got %q", cfg.HarnessConfig)
	}
	if cfg.Model != "claude-opus-4-6" {
		t.Errorf("expected Model='claude-opus-4-6', got %q", cfg.Model)
	}
	if cfg.Image != "my-image:latest" {
		t.Errorf("expected Image='my-image:latest', got %q", cfg.Image)
	}
	if cfg.User != "scion" {
		t.Errorf("expected User='scion', got %q", cfg.User)
	}
	if cfg.Task != "Review the code" {
		t.Errorf("expected Task='Review the code', got %q", cfg.Task)
	}
	if cfg.Branch != "review-branch" {
		t.Errorf("expected Branch='review-branch', got %q", cfg.Branch)
	}
	if !api.IntPtrEqual(cfg.MaxTurns, api.IntPtr(50)) {
		t.Errorf("expected MaxTurns=50, got %s", api.FormatCountLimit(cfg.MaxTurns))
	}
	if cfg.MaxDuration != "30m" {
		t.Errorf("expected MaxDuration='30m', got %q", cfg.MaxDuration)
	}
	if cfg.SystemPrompt != "You are a code reviewer.\n" {
		t.Errorf("expected SystemPrompt='You are a code reviewer.\\n', got %q", cfg.SystemPrompt)
	}
	if cfg.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("expected Env[LOG_LEVEL]='debug', got %q", cfg.Env["LOG_LEVEL"])
	}
}

func TestLoadInlineConfig_JSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	jsonContent := `{
  "harness_config": "claude-default",
  "model": "claude-opus-4-6",
  "user": "scion",
  "task": "Fix the bug",
  "branch": "fix-branch"
}`
	if err := os.WriteFile(cfgPath, []byte(jsonContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, _, err := loadInlineConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.HarnessConfig != "claude-default" {
		t.Errorf("expected HarnessConfig='claude-default', got %q", cfg.HarnessConfig)
	}
	if cfg.User != "scion" {
		t.Errorf("expected User='scion', got %q", cfg.User)
	}
	if cfg.Task != "Fix the bug" {
		t.Errorf("expected Task='Fix the bug', got %q", cfg.Task)
	}
	if cfg.Branch != "fix-branch" {
		t.Errorf("expected Branch='fix-branch', got %q", cfg.Branch)
	}
}

func TestLoadInlineConfig_Empty(t *testing.T) {
	cfg, _, err := loadInlineConfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil config for empty path, got %+v", cfg)
	}
}

func TestLoadInlineConfig_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(cfgPath, []byte(""), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	_, _, err := loadInlineConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
}

func TestLoadInlineConfig_NonexistentFile(t *testing.T) {
	_, _, err := loadInlineConfig("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}
}

func TestResolveInlineConfigContent_FileURI(t *testing.T) {
	dir := t.TempDir()

	// Create content files
	promptContent := "You are a helpful assistant."
	instructionsContent := "Follow these rules."

	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(promptContent), 0644); err != nil {
		t.Fatalf("failed to write prompt file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte(instructionsContent), 0644); err != nil {
		t.Fatalf("failed to write instructions file: %v", err)
	}

	cfg := &api.ScionConfig{
		SystemPrompt:      "file://prompt.md",
		AgentInstructions: "file://instructions.md",
	}

	if err := resolveInlineConfigContent(cfg, dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.SystemPrompt != promptContent {
		t.Errorf("expected SystemPrompt=%q, got %q", promptContent, cfg.SystemPrompt)
	}
	if cfg.AgentInstructions != instructionsContent {
		t.Errorf("expected AgentInstructions=%q, got %q", instructionsContent, cfg.AgentInstructions)
	}
}

func TestResolveInlineConfigContent_InlineContent(t *testing.T) {
	cfg := &api.ScionConfig{
		SystemPrompt:      "You are a code reviewer.",
		AgentInstructions: "Review the code carefully.",
	}

	if err := resolveInlineConfigContent(cfg, "/tmp"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.SystemPrompt != "You are a code reviewer." {
		t.Errorf("expected SystemPrompt preserved, got %q", cfg.SystemPrompt)
	}
	if cfg.AgentInstructions != "Review the code carefully." {
		t.Errorf("expected AgentInstructions preserved, got %q", cfg.AgentInstructions)
	}
}
