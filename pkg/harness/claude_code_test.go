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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestClaudeCode_GetCommand(t *testing.T) {
	c := &ClaudeCode{}

	// 1. Normal task
	cmd := c.GetCommand("do something", false, nil)
	expected := []string{"claude", "--no-chrome", "--dangerously-skip-permissions", "do something"}
	if !reflect.DeepEqual(cmd, expected) {
		t.Errorf("expected %v, got %v", expected, cmd)
	}

	// 2. Empty task
	cmd = c.GetCommand("", false, nil)
	expected = []string{"claude", "--no-chrome", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual(cmd, expected) {
		t.Errorf("expected %v, got %v", expected, cmd)
	}

	// 3. Resume
	cmd = c.GetCommand("do something", true, nil)
	expected = []string{"claude", "--no-chrome", "--dangerously-skip-permissions", "--continue", "do something"}
	if !reflect.DeepEqual(cmd, expected) {
		t.Errorf("expected %v, got %v", expected, cmd)
	}

	// 4. Task with baseArgs
	cmd = c.GetCommand("do something", false, []string{"--foo", "bar"})
	expected = []string{"claude", "--no-chrome", "--dangerously-skip-permissions", "--foo", "bar", "do something"}
	if !reflect.DeepEqual(cmd, expected) {
		t.Errorf("expected %v, got %v", expected, cmd)
	}

	// 5. With Model (via baseArgs)
	cmd = c.GetCommand("do something", false, []string{"--model", "claude-3-opus"})
	expected = []string{"claude", "--no-chrome", "--dangerously-skip-permissions", "--model", "claude-3-opus", "do something"}
	if !reflect.DeepEqual(cmd, expected) {
		t.Errorf("expected %v, got %v", expected, cmd)
	}
}

func TestClaudeCode_Provision(t *testing.T) {
	tmpDir := t.TempDir()
	agentHome := filepath.Join(tmpDir, "home")
	agentWorkspace := filepath.Join(tmpDir, "workspace")
	os.MkdirAll(agentHome, 0755)
	os.MkdirAll(agentWorkspace, 0755)

	claudeJSONPath := filepath.Join(agentHome, ".claude.json")
	initialCfg := map[string]interface{}{
		"projects": map[string]interface{}{
			"/old/path": map[string]interface{}{
				"allowedTools": []interface{}{"test-tool"},
			},
		},
	}
	data, _ := json.Marshal(initialCfg)
	os.WriteFile(claudeJSONPath, data, 0644)

	c := &ClaudeCode{}
	// Note: Provision uses util.RepoRoot() which might return an error or different path
	// depending on where tests run. In a real environment it would be more predictable.
	err := c.Provision(context.Background(), "test-agent", tmpDir, agentHome, agentWorkspace)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	// Verify .claude.json was updated
	updatedData, err := os.ReadFile(claudeJSONPath)
	if err != nil {
		t.Fatal(err)
	}

	var updatedCfg map[string]interface{}
	json.Unmarshal(updatedData, &updatedCfg)

	projects, ok := updatedCfg["projects"].(map[string]interface{})
	if !ok {
		t.Fatal("projects map not found in updated config")
	}

	// It should have one project entry, we don't strictly check the key because it depends on util.RepoRoot
	if len(projects) != 1 {
		t.Errorf("expected 1 project entry, got %d", len(projects))
	}

	for _, v := range projects {
		settings := v.(map[string]interface{})
		if settings["allowedTools"].([]interface{})[0] != "test-tool" {
			t.Errorf("expected preserved allowedTools, got %v", settings["allowedTools"])
		}
	}
}

func TestClaudeCode_Provision_VertexAI(t *testing.T) {
	tmpDir := t.TempDir()
	agentDir := tmpDir
	agentHome := filepath.Join(agentDir, "home")
	agentWorkspace := filepath.Join(tmpDir, "workspace")
	os.MkdirAll(agentHome, 0755)
	os.MkdirAll(agentWorkspace, 0755)

	// Create .claude.json so provisionClaudeJSON doesn't skip
	claudeJSONPath := filepath.Join(agentHome, ".claude.json")
	os.WriteFile(claudeJSONPath, []byte(`{"projects":{}}`), 0644)

	// Create scion-agent.json with vertex-ai auth type
	scionAgentPath := filepath.Join(agentDir, "scion-agent.json")
	scionCfg := api.ScionConfig{
		AuthSelectedType: "vertex-ai",
	}
	data, _ := json.MarshalIndent(scionCfg, "", "  ")
	os.WriteFile(scionAgentPath, data, 0644)

	c := &ClaudeCode{}
	err := c.Provision(context.Background(), "test-agent", agentDir, agentHome, agentWorkspace)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	// Read back scion-agent.json and verify env projections
	updated, err := os.ReadFile(scionAgentPath)
	if err != nil {
		t.Fatal(err)
	}
	var updatedCfg api.ScionConfig
	if err := json.Unmarshal(updated, &updatedCfg); err != nil {
		t.Fatal(err)
	}

	// Verify harness-specific env entries
	if updatedCfg.Env["CLAUDE_CODE_USE_VERTEX"] != "1" {
		t.Errorf("CLAUDE_CODE_USE_VERTEX = %q, want %q", updatedCfg.Env["CLAUDE_CODE_USE_VERTEX"], "1")
	}
	if updatedCfg.Env["ANTHROPIC_VERTEX_PROJECT_ID"] != "${GOOGLE_CLOUD_PROJECT}" {
		t.Errorf("ANTHROPIC_VERTEX_PROJECT_ID = %q, want %q", updatedCfg.Env["ANTHROPIC_VERTEX_PROJECT_ID"], "${GOOGLE_CLOUD_PROJECT}")
	}
	if updatedCfg.Env["CLOUD_ML_REGION"] != "${GOOGLE_CLOUD_REGION}" {
		t.Errorf("CLOUD_ML_REGION = %q, want %q", updatedCfg.Env["CLOUD_ML_REGION"], "${GOOGLE_CLOUD_REGION}")
	}

	// The gcloud volume mount must NOT be added by the harness — it is
	// handled by buildCommonRunArgs (gated on !BrokerMode) so that broker
	// mode does not leak the operator's credentials.
	for _, v := range updatedCfg.Volumes {
		if strings.Contains(v.Target, ".config/gcloud") {
			t.Error("harness should not add gcloud volume; mount is handled by buildCommonRunArgs")
		}
	}
}

func TestClaudeCode_GetTelemetryEnv(t *testing.T) {
	c := &ClaudeCode{}
	env := c.GetTelemetryEnv()

	expected := map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		"OTEL_METRICS_EXPORTER":        "otlp",
		"OTEL_LOGS_EXPORTER":           "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL":  "grpc",
		"OTEL_EXPORTER_OTLP_ENDPOINT":  "http://localhost:4317",
		"OTEL_METRIC_EXPORT_INTERVAL":  "30000",
	}

	if len(env) != len(expected) {
		t.Fatalf("expected %d env vars, got %d: %v", len(expected), len(env), env)
	}

	for k, want := range expected {
		got, ok := env[k]
		if !ok {
			t.Errorf("missing env var %s", k)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestClaudeInjectAgentInstructions(t *testing.T) {
	agentHome := t.TempDir()
	c := &ClaudeCode{}
	content := []byte("# Agent Instructions\nDo good work.")

	if err := c.InjectAgentInstructions(agentHome, content); err != nil {
		t.Fatalf("InjectAgentInstructions failed: %v", err)
	}

	target := filepath.Join(agentHome, ".claude", "CLAUDE.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected file at %s: %v", target, err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", string(data), string(content))
	}
}

func TestClaudeInjectAgentInstructions_RemovesLowercaseFile(t *testing.T) {
	agentHome := t.TempDir()
	c := &ClaudeCode{}

	// Simulate a harness-config home that provides claude.md (lowercase)
	claudeDir := filepath.Join(agentHome, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatal(err)
	}
	lowercasePath := filepath.Join(claudeDir, "claude.md")
	if err := os.WriteFile(lowercasePath, []byte("# Harness config instructions"), 0644); err != nil {
		t.Fatal(err)
	}

	// Inject agent instructions — should remove the lowercase file
	content := []byte("# Template Instructions\nFrom agents.md")
	if err := c.InjectAgentInstructions(agentHome, content); err != nil {
		t.Fatalf("InjectAgentInstructions failed: %v", err)
	}

	// Canonical CLAUDE.md should exist with the injected content
	target := filepath.Join(claudeDir, "CLAUDE.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected CLAUDE.md at %s: %v", target, err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", string(data), string(content))
	}

	// Lowercase claude.md should no longer exist (on case-sensitive filesystems)
	if _, err := os.Lstat(lowercasePath); err == nil {
		// File still exists — check if this is a case-insensitive FS
		// by comparing the name from directory listing
		entries, _ := os.ReadDir(claudeDir)
		for _, e := range entries {
			if strings.EqualFold(e.Name(), "CLAUDE.md") && e.Name() != "CLAUDE.md" {
				t.Errorf("lowercase %q should have been removed", e.Name())
			}
		}
	}
}

func TestClaudeInjectSystemPrompt(t *testing.T) {
	agentHome := t.TempDir()
	c := &ClaudeCode{}
	content := []byte("You are a helpful coding assistant.")

	if err := c.InjectSystemPrompt(agentHome, content); err != nil {
		t.Fatalf("InjectSystemPrompt failed: %v", err)
	}

	target := filepath.Join(agentHome, ".claude", "system-prompt.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected file at %s: %v", target, err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", string(data), string(content))
	}
}

func TestClaudeHasSystemPrompt(t *testing.T) {
	t.Run("no file", func(t *testing.T) {
		agentHome := t.TempDir()
		c := &ClaudeCode{}
		if c.HasSystemPrompt(agentHome) {
			t.Error("expected HasSystemPrompt=false when no file exists")
		}
	})

	t.Run("placeholder content", func(t *testing.T) {
		agentHome := t.TempDir()
		c := &ClaudeCode{}
		c.InjectSystemPrompt(agentHome, []byte("# Placeholder"))
		if c.HasSystemPrompt(agentHome) {
			t.Error("expected HasSystemPrompt=false for placeholder content")
		}
	})

	t.Run("empty content", func(t *testing.T) {
		agentHome := t.TempDir()
		c := &ClaudeCode{}
		c.InjectSystemPrompt(agentHome, []byte(""))
		if c.HasSystemPrompt(agentHome) {
			t.Error("expected HasSystemPrompt=false for empty content")
		}
	})

	t.Run("whitespace only", func(t *testing.T) {
		agentHome := t.TempDir()
		c := &ClaudeCode{}
		c.InjectSystemPrompt(agentHome, []byte("  \n\n  "))
		if c.HasSystemPrompt(agentHome) {
			t.Error("expected HasSystemPrompt=false for whitespace-only content")
		}
	})

	t.Run("valid content", func(t *testing.T) {
		agentHome := t.TempDir()
		c := &ClaudeCode{}
		c.InjectSystemPrompt(agentHome, []byte("You are a coding assistant."))
		if !c.HasSystemPrompt(agentHome) {
			t.Error("expected HasSystemPrompt=true for valid content")
		}
	})

	t.Run("empty agentHome", func(t *testing.T) {
		c := &ClaudeCode{}
		if c.HasSystemPrompt("") {
			t.Error("expected HasSystemPrompt=false for empty agentHome")
		}
	})
}

func TestClaudeGetCommand_WithSystemPrompt(t *testing.T) {
	agentHome := t.TempDir()
	c := &ClaudeCode{}

	// Inject a real system prompt
	c.InjectSystemPrompt(agentHome, []byte("You are a coding assistant."))

	// Load system prompt via GetEnv (simulates runtime flow)
	c.GetEnv("test-agent", agentHome, "scion")

	// GetCommand should now include --system-prompt
	cmd := c.GetCommand("do something", false, nil)
	expected := []string{
		"claude", "--no-chrome", "--dangerously-skip-permissions",
		"--system-prompt", "You are a coding assistant.",
		"do something",
	}
	if !reflect.DeepEqual(cmd, expected) {
		t.Errorf("expected %v, got %v", expected, cmd)
	}
}

func TestClaudeGetCommand_WithoutSystemPrompt(t *testing.T) {
	agentHome := t.TempDir()
	c := &ClaudeCode{}

	// Inject only placeholder content
	c.InjectSystemPrompt(agentHome, []byte("# Placeholder"))

	// Load via GetEnv — should not pick up placeholder
	c.GetEnv("test-agent", agentHome, "scion")

	cmd := c.GetCommand("do something", false, nil)
	expected := []string{"claude", "--no-chrome", "--dangerously-skip-permissions", "do something"}
	if !reflect.DeepEqual(cmd, expected) {
		t.Errorf("expected %v, got %v", expected, cmd)
	}
}

func TestClaudeGetCommand_SystemPromptWithBaseArgs(t *testing.T) {
	agentHome := t.TempDir()
	c := &ClaudeCode{}

	c.InjectSystemPrompt(agentHome, []byte("Be helpful."))
	c.GetEnv("test-agent", agentHome, "scion")

	cmd := c.GetCommand("task", false, []string{"--model", "opus"})
	expected := []string{
		"claude", "--no-chrome", "--dangerously-skip-permissions",
		"--system-prompt", "Be helpful.",
		"--model", "opus",
		"task",
	}
	if !reflect.DeepEqual(cmd, expected) {
		t.Errorf("expected %v, got %v", expected, cmd)
	}
}

func TestClaudeResolveAuth_APIKey(t *testing.T) {
	c := &ClaudeCode{}
	auth := api.AuthConfig{AnthropicAPIKey: "sk-ant-test"}
	result, err := c.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "api-key" {
		t.Errorf("Method = %q, want %q", result.Method, "api-key")
	}
	if result.EnvVars["ANTHROPIC_API_KEY"] != "sk-ant-test" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want %q", result.EnvVars["ANTHROPIC_API_KEY"], "sk-ant-test")
	}
	if len(result.Files) != 0 {
		t.Errorf("expected no files, got %d", len(result.Files))
	}
}

func TestClaudeResolveAuth_OAuthToken(t *testing.T) {
	c := &ClaudeCode{}
	auth := api.AuthConfig{ClaudeOAuthToken: "cco_test-token-123"}
	result, err := c.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "oauth-token" {
		t.Errorf("Method = %q, want %q", result.Method, "oauth-token")
	}
	if result.EnvVars["CLAUDE_CODE_OAUTH_TOKEN"] != "cco_test-token-123" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q, want %q", result.EnvVars["CLAUDE_CODE_OAUTH_TOKEN"], "cco_test-token-123")
	}
	if len(result.Files) != 0 {
		t.Errorf("expected no files, got %d", len(result.Files))
	}
}

func TestClaudeResolveAuth_OAuthToken_Explicit(t *testing.T) {
	c := &ClaudeCode{}
	auth := api.AuthConfig{
		SelectedType:     "oauth-token",
		ClaudeOAuthToken: "cco_explicit-token",
	}
	result, err := c.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "oauth-token" {
		t.Errorf("Method = %q, want %q", result.Method, "oauth-token")
	}
}

func TestClaudeResolveAuth_OAuthToken_ExplicitMissing(t *testing.T) {
	c := &ClaudeCode{}
	auth := api.AuthConfig{SelectedType: "oauth-token"}
	_, err := c.ResolveAuth(auth)
	if err == nil {
		t.Fatal("expected error for explicit oauth-token with no token")
	}
	if !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("error should mention CLAUDE_CODE_OAUTH_TOKEN: %v", err)
	}
}

func TestClaudeResolveAuth_APIKeyWinsOverOAuth(t *testing.T) {
	c := &ClaudeCode{}
	auth := api.AuthConfig{
		AnthropicAPIKey:  "sk-ant-key",
		ClaudeOAuthToken: "cco_token",
	}
	result, err := c.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "api-key" {
		t.Errorf("API key should win over OAuth token; Method = %q, want %q", result.Method, "api-key")
	}
}

func TestClaudeResolveAuth_OAuthWinsOverVertex(t *testing.T) {
	c := &ClaudeCode{}
	auth := api.AuthConfig{
		ClaudeOAuthToken:     "cco_token",
		GoogleAppCredentials: "/path/to/adc.json",
		GoogleCloudProject:   "my-project",
		GoogleCloudRegion:    "us-central1",
	}
	result, err := c.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "oauth-token" {
		t.Errorf("OAuth token should win over Vertex; Method = %q, want %q", result.Method, "oauth-token")
	}
}

func TestClaudeResolveAuth_VertexAI(t *testing.T) {
	c := &ClaudeCode{}
	auth := api.AuthConfig{
		GoogleAppCredentials: "/path/to/adc.json",
		GoogleCloudProject:   "my-project",
		GoogleCloudRegion:    "us-central1",
	}
	result, err := c.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "vertex-ai" {
		t.Errorf("Method = %q, want %q", result.Method, "vertex-ai")
	}
	if result.EnvVars["CLAUDE_CODE_USE_VERTEX"] != "1" {
		t.Errorf("CLAUDE_CODE_USE_VERTEX = %q, want %q", result.EnvVars["CLAUDE_CODE_USE_VERTEX"], "1")
	}
	if result.EnvVars["CLOUD_ML_REGION"] != "us-central1" {
		t.Errorf("CLOUD_ML_REGION = %q, want %q", result.EnvVars["CLOUD_ML_REGION"], "us-central1")
	}
	if result.EnvVars["ANTHROPIC_VERTEX_PROJECT_ID"] != "my-project" {
		t.Errorf("ANTHROPIC_VERTEX_PROJECT_ID = %q, want %q", result.EnvVars["ANTHROPIC_VERTEX_PROJECT_ID"], "my-project")
	}
	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file mapping, got %d", len(result.Files))
	}
	if result.Files[0].SourcePath != "/path/to/adc.json" {
		t.Errorf("SourcePath = %q, want %q", result.Files[0].SourcePath, "/path/to/adc.json")
	}
}

func TestClaudeResolveAuth_APIKeyWinsOverVertex(t *testing.T) {
	c := &ClaudeCode{}
	auth := api.AuthConfig{
		AnthropicAPIKey:      "sk-ant-key",
		GoogleAppCredentials: "/path/to/adc.json",
		GoogleCloudProject:   "my-project",
		GoogleCloudRegion:    "us-central1",
	}
	result, err := c.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "api-key" {
		t.Errorf("API key should win over Vertex; Method = %q, want %q", result.Method, "api-key")
	}
}

func TestClaudeResolveAuth_PartialVertex(t *testing.T) {
	c := &ClaudeCode{}

	// Missing region
	auth := api.AuthConfig{
		GoogleAppCredentials: "/path/to/adc.json",
		GoogleCloudProject:   "my-project",
	}
	_, err := c.ResolveAuth(auth)
	if err == nil {
		t.Fatal("expected error for partial Vertex creds (missing region)")
	}

	// Missing project
	auth = api.AuthConfig{
		GoogleAppCredentials: "/path/to/adc.json",
		GoogleCloudRegion:    "us-central1",
	}
	_, err = c.ResolveAuth(auth)
	if err == nil {
		t.Fatal("expected error for partial Vertex creds (missing project)")
	}
}

func TestClaudeResolveAuth_NoCreds(t *testing.T) {
	c := &ClaudeCode{}
	_, err := c.ResolveAuth(api.AuthConfig{})
	if err == nil {
		t.Fatal("expected error for empty AuthConfig")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error should mention ANTHROPIC_API_KEY: %v", err)
	}
	if !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("error should mention CLAUDE_CODE_OAUTH_TOKEN: %v", err)
	}
}

func TestClaudeResolveAuth_VertexAI_GCPServiceAccount(t *testing.T) {
	c := &ClaudeCode{}
	auth := api.AuthConfig{
		GCPMetadataMode:    "assign",
		GoogleCloudProject: "my-project",
		GoogleCloudRegion:  "us-central1",
	}
	result, err := c.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "vertex-ai" {
		t.Errorf("Method = %q, want %q", result.Method, "vertex-ai")
	}
	if result.EnvVars["CLAUDE_CODE_USE_VERTEX"] != "1" {
		t.Errorf("CLAUDE_CODE_USE_VERTEX = %q, want %q", result.EnvVars["CLAUDE_CODE_USE_VERTEX"], "1")
	}
	if result.EnvVars["ANTHROPIC_VERTEX_PROJECT_ID"] != "my-project" {
		t.Errorf("ANTHROPIC_VERTEX_PROJECT_ID = %q, want %q", result.EnvVars["ANTHROPIC_VERTEX_PROJECT_ID"], "my-project")
	}
	// No ADC file should be mapped — metadata server provides credentials
	if len(result.Files) != 0 {
		t.Errorf("expected no file mappings for GCP SA auth, got %d", len(result.Files))
	}
}

func TestClaudeResolveAuth_VertexAI_ExplicitWithGCPSA(t *testing.T) {
	c := &ClaudeCode{}
	auth := api.AuthConfig{
		SelectedType:       "vertex-ai",
		GCPMetadataMode:    "assign",
		GoogleCloudProject: "my-project",
		GoogleCloudRegion:  "us-central1",
	}
	result, err := c.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "vertex-ai" {
		t.Errorf("Method = %q, want %q", result.Method, "vertex-ai")
	}
	if len(result.Files) != 0 {
		t.Errorf("expected no file mappings for GCP SA auth, got %d", len(result.Files))
	}
}

func TestClaudeResolveAuth_APIKeyWinsOverGCPSA(t *testing.T) {
	c := &ClaudeCode{}
	auth := api.AuthConfig{
		AnthropicAPIKey:    "sk-ant-key",
		GCPMetadataMode:    "assign",
		GoogleCloudProject: "my-project",
		GoogleCloudRegion:  "us-central1",
	}
	result, err := c.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "api-key" {
		t.Errorf("API key should win over GCP SA; Method = %q, want %q", result.Method, "api-key")
	}
}

func TestClaudeApplyAuthSettings_APIKey(t *testing.T) {
	agentHome := t.TempDir()
	c := &ClaudeCode{}

	// Seed an existing .claude.json
	claudeJSONPath := filepath.Join(agentHome, ".claude.json")
	os.WriteFile(claudeJSONPath, []byte(`{"numStartups":1}`), 0644)

	apiKey := "REMOVED_API_KEY"
	resolved := &api.ResolvedAuth{
		Method:  "api-key",
		EnvVars: map[string]string{"ANTHROPIC_API_KEY": apiKey},
	}

	if err := c.ApplyAuthSettings(agentHome, resolved); err != nil {
		t.Fatalf("ApplyAuthSettings failed: %v", err)
	}

	data, err := os.ReadFile(claudeJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}

	// Verify customApiKeyResponses was added
	responses, ok := cfg["customApiKeyResponses"].(map[string]interface{})
	if !ok {
		t.Fatal("customApiKeyResponses not found or wrong type")
	}
	approved, ok := responses["approved"].([]interface{})
	if !ok || len(approved) != 1 {
		t.Fatalf("expected 1 approved entry, got %v", responses["approved"])
	}
	// Key is shorter than 20 chars, so the whole key is used as fingerprint
	want := "REMOVED_API_KEY"
	if approved[0] != want {
		t.Errorf("approved fingerprint = %q, want %q", approved[0], want)
	}

	// Verify existing fields preserved
	if cfg["numStartups"] != float64(1) {
		t.Errorf("existing field numStartups not preserved: %v", cfg["numStartups"])
	}
}

func TestClaudeApplyAuthSettings_APIKey_NoExistingFile(t *testing.T) {
	agentHome := t.TempDir()
	c := &ClaudeCode{}

	resolved := &api.ResolvedAuth{
		Method:  "api-key",
		EnvVars: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-short-key12345"},
	}

	if err := c.ApplyAuthSettings(agentHome, resolved); err != nil {
		t.Fatalf("ApplyAuthSettings failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(agentHome, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	json.Unmarshal(data, &cfg)

	responses := cfg["customApiKeyResponses"].(map[string]interface{})
	approved := responses["approved"].([]interface{})
	if approved[0] != "k-ant-short-key12345" {
		t.Errorf("fingerprint = %q, want %q", approved[0], "k-ant-short-key12345")
	}
}

func TestClaudeApplyAuthSettings_ShortKey(t *testing.T) {
	agentHome := t.TempDir()
	c := &ClaudeCode{}

	os.WriteFile(filepath.Join(agentHome, ".claude.json"), []byte(`{}`), 0644)

	// Key shorter than 20 chars — use the whole key
	resolved := &api.ResolvedAuth{
		Method:  "api-key",
		EnvVars: map[string]string{"ANTHROPIC_API_KEY": "short"},
	}

	if err := c.ApplyAuthSettings(agentHome, resolved); err != nil {
		t.Fatalf("ApplyAuthSettings failed: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(agentHome, ".claude.json"))
	var cfg map[string]interface{}
	json.Unmarshal(data, &cfg)

	approved := cfg["customApiKeyResponses"].(map[string]interface{})["approved"].([]interface{})
	if approved[0] != "short" {
		t.Errorf("fingerprint = %q, want %q", approved[0], "short")
	}
}

func TestClaudeApplyAuthSettings_VertexAI_Noop(t *testing.T) {
	agentHome := t.TempDir()
	c := &ClaudeCode{}

	claudeJSONPath := filepath.Join(agentHome, ".claude.json")
	os.WriteFile(claudeJSONPath, []byte(`{"numStartups":1}`), 0644)

	resolved := &api.ResolvedAuth{
		Method: "vertex-ai",
		EnvVars: map[string]string{
			"CLAUDE_CODE_USE_VERTEX": "1",
		},
	}

	if err := c.ApplyAuthSettings(agentHome, resolved); err != nil {
		t.Fatalf("ApplyAuthSettings failed: %v", err)
	}

	data, _ := os.ReadFile(claudeJSONPath)
	var cfg map[string]interface{}
	json.Unmarshal(data, &cfg)

	if _, exists := cfg["customApiKeyResponses"]; exists {
		t.Error("customApiKeyResponses should not be set for vertex-ai")
	}
}

func TestClaudeGetCommand_SystemPromptWithResume(t *testing.T) {
	agentHome := t.TempDir()
	c := &ClaudeCode{}

	c.InjectSystemPrompt(agentHome, []byte("Be concise."))
	c.GetEnv("test-agent", agentHome, "scion")

	cmd := c.GetCommand("task", true, nil)
	expected := []string{
		"claude", "--no-chrome", "--dangerously-skip-permissions",
		"--continue",
		"--system-prompt", "Be concise.",
		"task",
	}
	if !reflect.DeepEqual(cmd, expected) {
		t.Errorf("expected %v, got %v", expected, cmd)
	}
}
