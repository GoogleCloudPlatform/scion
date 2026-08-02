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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestGeneric_Name(t *testing.T) {
	g := &Generic{}
	if g.Name() != "generic" {
		t.Errorf("Expected name 'generic', got '%s'", g.Name())
	}
}

func TestGeneric_GetEnv(t *testing.T) {
	g := &Generic{}

	env := g.GetEnv("test-agent", "", "test-user")

	// GetEnv should only return non-auth env vars
	if env["SCION_AGENT_NAME"] != "test-agent" {
		t.Errorf("Expected SCION_AGENT_NAME = 'test-agent', got '%s'", env["SCION_AGENT_NAME"])
	}
}

func TestGeneric_GetCommand(t *testing.T) {
	g := &Generic{}

	cmd := g.GetCommand("test task", false, nil)
	if !reflect.DeepEqual(cmd, []string{"test task"}) {
		t.Errorf("Expected command ['test task'], got %v", cmd)
	}

	cmdWithArgs := g.GetCommand("test task", false, []string{"--arg1"})
	if !reflect.DeepEqual(cmdWithArgs, []string{"--arg1", "test task"}) {
		t.Errorf("Expected command ['--arg1', 'test task'], got %v", cmdWithArgs)
	}

	cmdEmpty := g.GetCommand("", false, nil)
	if len(cmdEmpty) != 0 {
		t.Errorf("Expected empty command, got %v", cmdEmpty)
	}
}

func TestGeneric_DefaultConfigDir(t *testing.T) {
	g := &Generic{}
	if g.DefaultConfigDir() != ".scion" {
		t.Errorf("Expected DefaultConfigDir '.scion', got '%s'", g.DefaultConfigDir())
	}
}

func TestGenericInjectAgentInstructions(t *testing.T) {
	agentHome := t.TempDir()
	g := &Generic{}
	content := []byte("# Agent Instructions\nDo good work.")

	if err := g.InjectAgentInstructions(agentHome, content); err != nil {
		t.Fatalf("InjectAgentInstructions failed: %v", err)
	}

	target := filepath.Join(agentHome, "agents.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected file at %s: %v", target, err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", string(data), string(content))
	}
}

func TestGenericResolveAuth_EmptyConfig(t *testing.T) {
	g := &Generic{}
	result, err := g.ResolveAuth(api.AuthConfig{})
	if err != nil {
		t.Fatalf("generic should never error: %v", err)
	}
	if result.Method != "passthrough" {
		t.Errorf("Method = %q, want %q", result.Method, "passthrough")
	}
	if len(result.EnvVars) != 0 {
		t.Errorf("expected empty EnvVars for empty config, got %v", result.EnvVars)
	}
	if len(result.Files) != 0 {
		t.Errorf("expected empty Files for empty config, got %d", len(result.Files))
	}
}

func TestGenericResolveAuth_AllCreds(t *testing.T) {
	g := &Generic{}
	auth := api.AuthConfig{
		GoogleCloudProject:   "proj",
		GoogleCloudRegion:    "region",
		GoogleAppCredentials: "/adc.json",
		EnvVars: map[string]string{
			"ANTHROPIC_API_KEY": "anthropic",
			"GEMINI_API_KEY":    "gemini",
			"GOOGLE_API_KEY":    "google",
			"OPENAI_API_KEY":    "openai",
			"CODEX_API_KEY":     "codex",
		},
	}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("generic should never error: %v", err)
	}

	expectedEnvVars := map[string]string{
		"ANTHROPIC_API_KEY":    "anthropic",
		"GEMINI_API_KEY":       "gemini",
		"GOOGLE_API_KEY":       "google",
		"OPENAI_API_KEY":       "openai",
		"CODEX_API_KEY":        "codex",
		"GOOGLE_CLOUD_PROJECT": "proj",
		"GOOGLE_CLOUD_REGION":  "region",
	}
	for k, want := range expectedEnvVars {
		if got := result.EnvVars[k]; got != want {
			t.Errorf("EnvVars[%q] = %q, want %q", k, got, want)
		}
	}

	// Should have 1 file mapping: ADC
	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file mapping (ADC), got %d", len(result.Files))
	}
}

func TestGenericResolveAuth_PartialCreds(t *testing.T) {
	g := &Generic{}
	auth := api.AuthConfig{
		EnvVars: map[string]string{"ANTHROPIC_API_KEY": "key-only"},
	}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("generic should never error: %v", err)
	}
	if result.EnvVars["ANTHROPIC_API_KEY"] != "key-only" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want %q", result.EnvVars["ANTHROPIC_API_KEY"], "key-only")
	}
	if len(result.EnvVars) != 1 {
		t.Errorf("expected 1 env var, got %d: %v", len(result.EnvVars), result.EnvVars)
	}
	if len(result.Files) != 0 {
		t.Errorf("expected no files, got %d", len(result.Files))
	}
}

func TestGenericInjectSystemPrompt(t *testing.T) {
	agentHome := t.TempDir()
	g := &Generic{}
	content := []byte("You are a helpful coding assistant.")

	if err := g.InjectSystemPrompt(agentHome, content); err != nil {
		t.Fatalf("InjectSystemPrompt failed: %v", err)
	}

	target := filepath.Join(agentHome, ".scion", "system_prompt.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected file at %s: %v", target, err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", string(data), string(content))
	}
}
