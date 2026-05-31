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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestOpenCodeInjectAgentInstructions(t *testing.T) {
	agentHome := t.TempDir()
	o := &OpenCode{}
	content := []byte("# Agent Instructions\nDo good work.")

	if err := o.InjectAgentInstructions(agentHome, content); err != nil {
		t.Fatalf("InjectAgentInstructions failed: %v", err)
	}

	target := filepath.Join(agentHome, "AGENTS.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected file at %s: %v", target, err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", string(data), string(content))
	}
}

func TestOpenCodeInjectSystemPrompt(t *testing.T) {
	agentHome := t.TempDir()
	o := &OpenCode{}

	// First inject agent instructions
	agentContent := []byte("# Existing Instructions\nDo things.")
	if err := o.InjectAgentInstructions(agentHome, agentContent); err != nil {
		t.Fatalf("InjectAgentInstructions failed: %v", err)
	}

	// Now inject system prompt (should prepend)
	sysContent := []byte("You are a helpful assistant.")
	if err := o.InjectSystemPrompt(agentHome, sysContent); err != nil {
		t.Fatalf("InjectSystemPrompt failed: %v", err)
	}

	target := filepath.Join(agentHome, o.DefaultConfigDir(), "AGENTS.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected file at %s: %v", target, err)
	}

	content := string(data)
	if !strings.Contains(content, "# System Prompt") {
		t.Error("expected system prompt header in merged content")
	}
	if !strings.Contains(content, "You are a helpful assistant.") {
		t.Error("expected system prompt content in merged file")
	}
	if !strings.Contains(content, "# Existing Instructions") {
		t.Error("expected original agent instructions to be preserved")
	}
}

func TestOpenCodeResolveAuth_AnthropicAPIKey(t *testing.T) {
	o := &OpenCode{}
	auth := api.AuthConfig{AnthropicAPIKey: "sk-ant-test"}
	result, err := o.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "api-key" {
		t.Errorf("Method = %q, want %q", result.Method, "api-key")
	}
	if result.EnvVars["ANTHROPIC_API_KEY"] != "sk-ant-test" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want %q", result.EnvVars["ANTHROPIC_API_KEY"], "sk-ant-test")
	}
}

func TestOpenCodeResolveAuth_OpenAIAPIKey(t *testing.T) {
	o := &OpenCode{}
	auth := api.AuthConfig{OpenAIAPIKey: "sk-openai-test"}
	result, err := o.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "api-key" {
		t.Errorf("Method = %q, want %q", result.Method, "api-key")
	}
	if result.EnvVars["OPENAI_API_KEY"] != "sk-openai-test" {
		t.Errorf("OPENAI_API_KEY = %q, want %q", result.EnvVars["OPENAI_API_KEY"], "sk-openai-test")
	}
}

func TestOpenCodeResolveAuth_AuthFile(t *testing.T) {
	o := &OpenCode{}
	auth := api.AuthConfig{OpenCodeAuthFile: "/home/user/.local/share/opencode/auth.json"}
	result, err := o.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "auth-file" {
		t.Errorf("Method = %q, want %q", result.Method, "auth-file")
	}
	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file mapping, got %d", len(result.Files))
	}
}

func TestOpenCodeResolveAuth_PreferenceOrder(t *testing.T) {
	o := &OpenCode{}
	// AnthropicAPIKey should win over OpenAIAPIKey and auth file
	auth := api.AuthConfig{
		AnthropicAPIKey:  "anthropic",
		OpenAIAPIKey:     "openai",
		OpenCodeAuthFile: "/auth.json",
	}
	result, err := o.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "api-key" {
		t.Errorf("AnthropicAPIKey should win; Method = %q, want %q", result.Method, "api-key")
	}

	// OpenAIAPIKey should win over auth file
	auth = api.AuthConfig{
		OpenAIAPIKey:     "openai",
		OpenCodeAuthFile: "/auth.json",
	}
	result, err = o.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "api-key" {
		t.Errorf("OpenAIAPIKey should win over auth file; Method = %q, want %q", result.Method, "api-key")
	}
}

func TestOpenCodeResolveAuth_NoCreds(t *testing.T) {
	o := &OpenCode{}
	_, err := o.ResolveAuth(api.AuthConfig{})
	if err == nil {
		t.Fatal("expected error for empty AuthConfig")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error should mention ANTHROPIC_API_KEY: %v", err)
	}
}

func TestOpenCodeInjectSystemPrompt_NoExistingInstructions(t *testing.T) {
	agentHome := t.TempDir()
	o := &OpenCode{}

	sysContent := []byte("You are a helpful assistant.")
	if err := o.InjectSystemPrompt(agentHome, sysContent); err != nil {
		t.Fatalf("InjectSystemPrompt failed: %v", err)
	}

	target := filepath.Join(agentHome, "AGENTS.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected file at %s: %v", target, err)
	}

	content := string(data)
	if !strings.Contains(content, "# System Prompt") {
		t.Error("expected system prompt header")
	}
	if !strings.Contains(content, "You are a helpful assistant.") {
		t.Error("expected system prompt content")
	}
}
