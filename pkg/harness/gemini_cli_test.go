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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestGeminiGetTelemetryEnv(t *testing.T) {
	g := &GeminiCLI{}
	env := g.GetTelemetryEnv()

	expected := map[string]string{
		"GEMINI_TELEMETRY_ENABLED":       "true",
		"GEMINI_TELEMETRY_TARGET":        "local",
		"GEMINI_TELEMETRY_USE_COLLECTOR": "true",
		"GEMINI_TELEMETRY_OTLP_ENDPOINT": "http://localhost:4317",
		"GEMINI_TELEMETRY_OTLP_PROTOCOL": "grpc",
		"GEMINI_TELEMETRY_LOG_PROMPTS":   "false",
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

func TestGeminiInjectAgentInstructions(t *testing.T) {
	agentHome := t.TempDir()
	g := &GeminiCLI{}
	content := []byte("# Agent Instructions\nDo good work.")

	if err := g.InjectAgentInstructions(agentHome, content); err != nil {
		t.Fatalf("InjectAgentInstructions failed: %v", err)
	}

	target := filepath.Join(agentHome, ".gemini", "GEMINI.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected file at %s: %v", target, err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", string(data), string(content))
	}
}

func TestGeminiInjectAgentInstructions_RemovesLowercaseFile(t *testing.T) {
	agentHome := t.TempDir()
	g := &GeminiCLI{}

	// Simulate a harness-config home that provides gemini.md (lowercase)
	geminiDir := filepath.Join(agentHome, ".gemini")
	if err := os.MkdirAll(geminiDir, 0755); err != nil {
		t.Fatal(err)
	}
	lowercasePath := filepath.Join(geminiDir, "gemini.md")
	if err := os.WriteFile(lowercasePath, []byte("# Harness config instructions"), 0644); err != nil {
		t.Fatal(err)
	}

	// Inject agent instructions — should remove the lowercase file
	content := []byte("# Template Instructions\nFrom agents.md")
	if err := g.InjectAgentInstructions(agentHome, content); err != nil {
		t.Fatalf("InjectAgentInstructions failed: %v", err)
	}

	// Canonical GEMINI.md should exist with the injected content
	target := filepath.Join(geminiDir, "GEMINI.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected GEMINI.md at %s: %v", target, err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", string(data), string(content))
	}

	// Lowercase gemini.md should no longer exist (on case-sensitive filesystems)
	if _, err := os.Lstat(lowercasePath); err == nil {
		entries, _ := os.ReadDir(geminiDir)
		for _, e := range entries {
			if strings.EqualFold(e.Name(), "GEMINI.md") && e.Name() != "GEMINI.md" {
				t.Errorf("lowercase %q should have been removed", e.Name())
			}
		}
	}
}

func TestGeminiResolveAuth_ExplicitAPIKey(t *testing.T) {
	g := &GeminiCLI{}
	auth := api.AuthConfig{
		SelectedType: "api-key",
		GeminiAPIKey: "test-key",
	}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "api-key" {
		t.Errorf("Method = %q, want %q", result.Method, "api-key")
	}
	if result.EnvVars["GEMINI_API_KEY"] != "test-key" {
		t.Errorf("GEMINI_API_KEY = %q, want %q", result.EnvVars["GEMINI_API_KEY"], "test-key")
	}
	if result.EnvVars["GEMINI_DEFAULT_AUTH_TYPE"] != "gemini-api-key" {
		t.Errorf("GEMINI_DEFAULT_AUTH_TYPE = %q, want %q", result.EnvVars["GEMINI_DEFAULT_AUTH_TYPE"], "gemini-api-key")
	}
}

func TestGeminiResolveAuth_ExplicitAPIKeyFallbackGoogle(t *testing.T) {
	g := &GeminiCLI{}
	auth := api.AuthConfig{
		SelectedType: "api-key",
		GoogleAPIKey: "google-key",
	}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.EnvVars["GOOGLE_API_KEY"] != "google-key" {
		t.Errorf("GOOGLE_API_KEY = %q, want %q", result.EnvVars["GOOGLE_API_KEY"], "google-key")
	}
}

func TestGeminiResolveAuth_ExplicitAPIKeyMissing(t *testing.T) {
	g := &GeminiCLI{}
	auth := api.AuthConfig{SelectedType: "api-key"}
	_, err := g.ResolveAuth(auth)
	if err == nil {
		t.Fatal("expected error for explicit api-key with no key")
	}
}

func TestGeminiResolveAuth_ExplicitAuthFileOAuth(t *testing.T) {
	g := &GeminiCLI{}
	auth := api.AuthConfig{
		SelectedType:       "auth-file",
		OAuthCreds:         "/path/to/oauth.json",
		GoogleCloudProject: "proj",
	}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "auth-file" {
		t.Errorf("Method = %q, want %q", result.Method, "auth-file")
	}
	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file mapping, got %d", len(result.Files))
	}
	if result.EnvVars["GOOGLE_CLOUD_PROJECT"] != "proj" {
		t.Errorf("GOOGLE_CLOUD_PROJECT = %q, want %q", result.EnvVars["GOOGLE_CLOUD_PROJECT"], "proj")
	}
}

func TestGeminiResolveAuth_ExplicitAuthFileADCOnly(t *testing.T) {
	g := &GeminiCLI{}
	// auth-file requires OAuth creds; ADC alone is not sufficient
	auth := api.AuthConfig{
		SelectedType:         "auth-file",
		GoogleAppCredentials: "/path/to/adc.json",
	}
	_, err := g.ResolveAuth(auth)
	if err == nil {
		t.Fatal("expected error for explicit auth-file with only ADC (no OAuth creds)")
	}
}

func TestGeminiResolveAuth_ExplicitAuthFileMissing(t *testing.T) {
	g := &GeminiCLI{}
	auth := api.AuthConfig{SelectedType: "auth-file"}
	_, err := g.ResolveAuth(auth)
	if err == nil {
		t.Fatal("expected error for explicit auth-file with no creds")
	}
}

func TestGeminiResolveAuth_ExplicitVertexAI(t *testing.T) {
	g := &GeminiCLI{}
	auth := api.AuthConfig{
		SelectedType:         "vertex-ai",
		GoogleCloudProject:   "proj",
		GoogleCloudRegion:    "us-east1",
		GoogleAppCredentials: "/path/to/adc.json",
	}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "vertex-ai" {
		t.Errorf("Method = %q, want %q", result.Method, "vertex-ai")
	}
	if result.EnvVars["GOOGLE_CLOUD_PROJECT"] != "proj" {
		t.Errorf("GOOGLE_CLOUD_PROJECT = %q, want %q", result.EnvVars["GOOGLE_CLOUD_PROJECT"], "proj")
	}
	if result.EnvVars["GOOGLE_CLOUD_REGION"] != "us-east1" {
		t.Errorf("GOOGLE_CLOUD_REGION = %q, want %q", result.EnvVars["GOOGLE_CLOUD_REGION"], "us-east1")
	}
	if result.EnvVars["GOOGLE_CLOUD_LOCATION"] != "us-east1" {
		t.Errorf("GOOGLE_CLOUD_LOCATION = %q, want %q", result.EnvVars["GOOGLE_CLOUD_LOCATION"], "us-east1")
	}
	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file mapping, got %d", len(result.Files))
	}
}

func TestGeminiResolveAuth_ExplicitVertexMissingProject(t *testing.T) {
	g := &GeminiCLI{}
	auth := api.AuthConfig{SelectedType: "vertex-ai"}
	_, err := g.ResolveAuth(auth)
	if err == nil {
		t.Fatal("expected error for explicit vertex-ai with no project")
	}
}

func TestGeminiResolveAuth_UnknownType(t *testing.T) {
	g := &GeminiCLI{}
	auth := api.AuthConfig{SelectedType: "foobar"}
	_, err := g.ResolveAuth(auth)
	if err == nil {
		t.Fatal("expected error for unknown selected type")
	}
	if !strings.Contains(err.Error(), "foobar") {
		t.Errorf("error should mention the unknown type: %v", err)
	}
}

func TestGeminiResolveAuth_AutoDetectAPIKey(t *testing.T) {
	g := &GeminiCLI{}
	auth := api.AuthConfig{GeminiAPIKey: "auto-key"}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "api-key" {
		t.Errorf("Method = %q, want %q", result.Method, "api-key")
	}
}

func TestGeminiResolveAuth_AutoDetectADCVertexAI(t *testing.T) {
	g := &GeminiCLI{}
	// ADC with cloud project auto-detects as vertex-ai
	auth := api.AuthConfig{
		GoogleAppCredentials: "/path/to/adc.json",
		GoogleCloudProject:   "my-project",
	}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "vertex-ai" {
		t.Errorf("Method = %q, want %q", result.Method, "vertex-ai")
	}
	if result.EnvVars["GEMINI_DEFAULT_AUTH_TYPE"] != "vertex-ai" {
		t.Errorf("GEMINI_DEFAULT_AUTH_TYPE = %q, want %q", result.EnvVars["GEMINI_DEFAULT_AUTH_TYPE"], "vertex-ai")
	}
	if result.EnvVars["GOOGLE_CLOUD_PROJECT"] != "my-project" {
		t.Errorf("GOOGLE_CLOUD_PROJECT = %q, want %q", result.EnvVars["GOOGLE_CLOUD_PROJECT"], "my-project")
	}
	if len(result.Files) != 1 || result.Files[0].ContainerPath != "~/.config/gcloud/application_default_credentials.json" {
		t.Errorf("expected ADC file mapping, got %v", result.Files)
	}
}

func TestGeminiResolveAuth_AutoDetectADCVertexAIWithRegion(t *testing.T) {
	g := &GeminiCLI{}
	auth := api.AuthConfig{
		GoogleAppCredentials: "/path/to/adc.json",
		GoogleCloudProject:   "my-project",
		GoogleCloudRegion:    "europe-west1",
	}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.EnvVars["GOOGLE_CLOUD_REGION"] != "europe-west1" {
		t.Errorf("GOOGLE_CLOUD_REGION = %q, want %q", result.EnvVars["GOOGLE_CLOUD_REGION"], "europe-west1")
	}
	if result.EnvVars["GOOGLE_CLOUD_LOCATION"] != "europe-west1" {
		t.Errorf("GOOGLE_CLOUD_LOCATION = %q, want %q", result.EnvVars["GOOGLE_CLOUD_LOCATION"], "europe-west1")
	}
}

func TestGeminiResolveAuth_AutoDetectADCWithoutProject(t *testing.T) {
	g := &GeminiCLI{}
	// ADC without cloud project should not auto-detect
	auth := api.AuthConfig{GoogleAppCredentials: "/path/to/adc.json"}
	_, err := g.ResolveAuth(auth)
	if err == nil {
		t.Fatal("expected error for auto-detect with ADC but no cloud project")
	}
}

func TestGeminiResolveAuth_AutoDetectOAuth(t *testing.T) {
	g := &GeminiCLI{}
	auth := api.AuthConfig{OAuthCreds: "/path/to/oauth.json"}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "auth-file" {
		t.Errorf("Method = %q, want %q", result.Method, "auth-file")
	}
	if result.EnvVars["GEMINI_DEFAULT_AUTH_TYPE"] != "oauth-personal" {
		t.Errorf("GEMINI_DEFAULT_AUTH_TYPE = %q, want %q", result.EnvVars["GEMINI_DEFAULT_AUTH_TYPE"], "oauth-personal")
	}
	if len(result.Files) != 1 || result.Files[0].ContainerPath != "~/.gemini/oauth_creds.json" {
		t.Errorf("expected file mapping to ~/.gemini/oauth_creds.json, got %v", result.Files)
	}
}

func TestGeminiResolveAuth_AutoDetectPriority(t *testing.T) {
	g := &GeminiCLI{}
	// API key should win over ADC and OAuth
	auth := api.AuthConfig{
		GeminiAPIKey:         "key",
		GoogleAppCredentials: "/adc.json",
		OAuthCreds:           "/oauth.json",
	}
	result, err := g.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Method != "api-key" {
		t.Errorf("API key should win; Method = %q, want %q", result.Method, "api-key")
	}
}

func TestGeminiResolveAuth_NoCreds(t *testing.T) {
	g := &GeminiCLI{}
	_, err := g.ResolveAuth(api.AuthConfig{})
	if err == nil {
		t.Fatal("expected error for empty AuthConfig")
	}
}

func TestGeminiApplyAuthSettings_OAuthPersonal(t *testing.T) {
	agentHome := t.TempDir()
	g := &GeminiCLI{}

	resolved := &api.ResolvedAuth{
		Method: "auth-file",
		EnvVars: map[string]string{
			"GEMINI_DEFAULT_AUTH_TYPE": "oauth-personal",
		},
	}
	if err := g.ApplyAuthSettings(agentHome, resolved); err != nil {
		t.Fatalf("ApplyAuthSettings failed: %v", err)
	}

	settingsPath := filepath.Join(agentHome, ".gemini", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("expected settings.json at %s: %v", settingsPath, err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}
	sec, _ := settings["security"].(map[string]interface{})
	auth, _ := sec["auth"].(map[string]interface{})
	if got := auth["selectedType"]; got != "oauth-personal" {
		t.Errorf("selectedType = %q, want %q", got, "oauth-personal")
	}
}

func TestGeminiInjectSystemPrompt(t *testing.T) {
	agentHome := t.TempDir()
	g := &GeminiCLI{}
	content := []byte("You are a helpful coding assistant.")

	if err := g.InjectSystemPrompt(agentHome, content); err != nil {
		t.Fatalf("InjectSystemPrompt failed: %v", err)
	}

	target := filepath.Join(agentHome, ".gemini", "system_prompt.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected file at %s: %v", target, err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", string(data), string(content))
	}
}
