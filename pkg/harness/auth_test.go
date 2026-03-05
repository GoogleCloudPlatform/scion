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

	"github.com/ptone/scion-agent/pkg/api"
)

func TestGatherAuth_EnvVars(t *testing.T) {
	// Set up all env vars
	t.Setenv("GEMINI_API_KEY", "gemini-key")
	t.Setenv("GOOGLE_API_KEY", "google-key")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("CODEX_API_KEY", "codex-key")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "my-project")
	t.Setenv("GOOGLE_CLOUD_REGION", "us-central1")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/path/to/creds.json")

	auth := GatherAuth()

	if auth.GeminiAPIKey != "gemini-key" {
		t.Errorf("GeminiAPIKey = %q, want %q", auth.GeminiAPIKey, "gemini-key")
	}
	if auth.GoogleAPIKey != "google-key" {
		t.Errorf("GoogleAPIKey = %q, want %q", auth.GoogleAPIKey, "google-key")
	}
	if auth.AnthropicAPIKey != "anthropic-key" {
		t.Errorf("AnthropicAPIKey = %q, want %q", auth.AnthropicAPIKey, "anthropic-key")
	}
	if auth.OpenAIAPIKey != "openai-key" {
		t.Errorf("OpenAIAPIKey = %q, want %q", auth.OpenAIAPIKey, "openai-key")
	}
	if auth.CodexAPIKey != "codex-key" {
		t.Errorf("CodexAPIKey = %q, want %q", auth.CodexAPIKey, "codex-key")
	}
	if auth.GoogleCloudProject != "my-project" {
		t.Errorf("GoogleCloudProject = %q, want %q", auth.GoogleCloudProject, "my-project")
	}
	if auth.GoogleCloudRegion != "us-central1" {
		t.Errorf("GoogleCloudRegion = %q, want %q", auth.GoogleCloudRegion, "us-central1")
	}
	if auth.GoogleAppCredentials != "/path/to/creds.json" {
		t.Errorf("GoogleAppCredentials = %q, want %q", auth.GoogleAppCredentials, "/path/to/creds.json")
	}
}

func TestGatherAuth_ProjectFallbacks(t *testing.T) {
	// Test GCP_PROJECT fallback
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCP_PROJECT", "gcp-proj")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")

	auth := GatherAuth()
	if auth.GoogleCloudProject != "gcp-proj" {
		t.Errorf("GoogleCloudProject = %q, want %q (GCP_PROJECT fallback)", auth.GoogleCloudProject, "gcp-proj")
	}

	// Test ANTHROPIC_VERTEX_PROJECT_ID fallback
	t.Setenv("GCP_PROJECT", "")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "vertex-proj")

	auth = GatherAuth()
	if auth.GoogleCloudProject != "vertex-proj" {
		t.Errorf("GoogleCloudProject = %q, want %q (ANTHROPIC_VERTEX_PROJECT_ID fallback)", auth.GoogleCloudProject, "vertex-proj")
	}
}

func TestGatherAuth_RegionFallbacks(t *testing.T) {
	// Test CLOUD_ML_REGION fallback
	t.Setenv("GOOGLE_CLOUD_REGION", "")
	t.Setenv("CLOUD_ML_REGION", "ml-region")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "")

	auth := GatherAuth()
	if auth.GoogleCloudRegion != "ml-region" {
		t.Errorf("GoogleCloudRegion = %q, want %q (CLOUD_ML_REGION fallback)", auth.GoogleCloudRegion, "ml-region")
	}

	// Test GOOGLE_CLOUD_LOCATION fallback
	t.Setenv("CLOUD_ML_REGION", "")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "location")

	auth = GatherAuth()
	if auth.GoogleCloudRegion != "location" {
		t.Errorf("GoogleCloudRegion = %q, want %q (GOOGLE_CLOUD_LOCATION fallback)", auth.GoogleCloudRegion, "location")
	}
}

func TestGatherAuth_FileDiscovery(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Clear env vars that would take precedence
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCP_PROJECT", "")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	t.Setenv("GOOGLE_CLOUD_REGION", "")
	t.Setenv("CLOUD_ML_REGION", "")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "")

	// Create well-known credential files
	adcPath := filepath.Join(tmpHome, ".config", "gcloud", "application_default_credentials.json")
	os.MkdirAll(filepath.Dir(adcPath), 0755)
	os.WriteFile(adcPath, []byte(`{"type":"authorized_user"}`), 0644)

	oauthPath := filepath.Join(tmpHome, ".gemini", "oauth_creds.json")
	os.MkdirAll(filepath.Dir(oauthPath), 0755)
	os.WriteFile(oauthPath, []byte(`{"dummy":"oauth"}`), 0644)

	codexPath := filepath.Join(tmpHome, ".codex", "auth.json")
	os.MkdirAll(filepath.Dir(codexPath), 0755)
	os.WriteFile(codexPath, []byte(`{"dummy":"codex"}`), 0644)

	opencodePath := filepath.Join(tmpHome, ".local", "share", "opencode", "auth.json")
	os.MkdirAll(filepath.Dir(opencodePath), 0755)
	os.WriteFile(opencodePath, []byte(`{"dummy":"opencode"}`), 0644)

	auth := GatherAuth()

	if auth.GoogleAppCredentials != adcPath {
		t.Errorf("GoogleAppCredentials = %q, want %q", auth.GoogleAppCredentials, adcPath)
	}
	if auth.OAuthCreds != oauthPath {
		t.Errorf("OAuthCreds = %q, want %q", auth.OAuthCreds, oauthPath)
	}
	if auth.CodexAuthFile != codexPath {
		t.Errorf("CodexAuthFile = %q, want %q", auth.CodexAuthFile, codexPath)
	}
	if auth.OpenCodeAuthFile != opencodePath {
		t.Errorf("OpenCodeAuthFile = %q, want %q", auth.OpenCodeAuthFile, opencodePath)
	}
}

func TestGatherAuth_EnvCredsTakePrecedenceOverFiles(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create the ADC file
	adcPath := filepath.Join(tmpHome, ".config", "gcloud", "application_default_credentials.json")
	os.MkdirAll(filepath.Dir(adcPath), 0755)
	os.WriteFile(adcPath, []byte(`{"type":"authorized_user"}`), 0644)

	// Set env var — should take precedence over file discovery
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/explicit/path/creds.json")

	auth := GatherAuth()
	if auth.GoogleAppCredentials != "/explicit/path/creds.json" {
		t.Errorf("GoogleAppCredentials = %q, want env value %q", auth.GoogleAppCredentials, "/explicit/path/creds.json")
	}
}

func TestGatherAuth_NoFiles(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Clear all env vars
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCP_PROJECT", "")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	t.Setenv("GOOGLE_CLOUD_REGION", "")
	t.Setenv("CLOUD_ML_REGION", "")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "")

	auth := GatherAuth()

	if auth.GoogleAppCredentials != "" {
		t.Errorf("GoogleAppCredentials = %q, want empty", auth.GoogleAppCredentials)
	}
	if auth.OAuthCreds != "" {
		t.Errorf("OAuthCreds = %q, want empty", auth.OAuthCreds)
	}
	if auth.CodexAuthFile != "" {
		t.Errorf("CodexAuthFile = %q, want empty", auth.CodexAuthFile)
	}
	if auth.OpenCodeAuthFile != "" {
		t.Errorf("OpenCodeAuthFile = %q, want empty", auth.OpenCodeAuthFile)
	}
}

func TestValidateAuth_Valid(t *testing.T) {
	resolved := &api.ResolvedAuth{
		Method: "anthropic-api-key",
		EnvVars: map[string]string{
			"ANTHROPIC_API_KEY": "sk-ant-test",
		},
	}
	if err := ValidateAuth(resolved); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateAuth_ValidWithFiles(t *testing.T) {
	// Create a temp file to serve as source
	tmpFile := filepath.Join(t.TempDir(), "creds.json")
	os.WriteFile(tmpFile, []byte(`{"type":"test"}`), 0644)

	resolved := &api.ResolvedAuth{
		Method: "vertex-ai",
		EnvVars: map[string]string{
			"CLAUDE_CODE_USE_VERTEX": "1",
		},
		Files: []api.FileMapping{
			{SourcePath: tmpFile, ContainerPath: "~/.config/gcp/adc.json"},
		},
	}
	if err := ValidateAuth(resolved); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateAuth_Nil(t *testing.T) {
	err := ValidateAuth(nil)
	if err == nil {
		t.Fatal("expected error for nil resolved auth")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("error should mention nil: %v", err)
	}
}

func TestValidateAuth_EmptyMethod(t *testing.T) {
	resolved := &api.ResolvedAuth{
		Method:  "",
		EnvVars: map[string]string{"KEY": "value"},
	}
	err := ValidateAuth(resolved)
	if err == nil {
		t.Fatal("expected error for empty method")
	}
	if !strings.Contains(err.Error(), "no auth method") {
		t.Errorf("error should mention missing method: %v", err)
	}
}

func TestValidateAuth_EmptyEnvValue(t *testing.T) {
	resolved := &api.ResolvedAuth{
		Method: "test-method",
		EnvVars: map[string]string{
			"GOOD_KEY":  "value",
			"EMPTY_KEY": "",
		},
	}
	err := ValidateAuth(resolved)
	if err == nil {
		t.Fatal("expected error for empty env var value")
	}
	if !strings.Contains(err.Error(), "EMPTY_KEY") {
		t.Errorf("error should mention EMPTY_KEY: %v", err)
	}
}

func TestValidateAuth_MissingSourceFile(t *testing.T) {
	resolved := &api.ResolvedAuth{
		Method: "vertex-ai",
		Files: []api.FileMapping{
			{SourcePath: "/nonexistent/path/creds.json", ContainerPath: "~/.config/gcp/adc.json"},
		},
	}
	err := ValidateAuth(resolved)
	if err == nil {
		t.Fatal("expected error for missing source file")
	}
	if !strings.Contains(err.Error(), "/nonexistent/path/creds.json") {
		t.Errorf("error should mention the missing file path: %v", err)
	}
}

func TestValidateAuth_EmptyContainerPath(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "creds.json")
	os.WriteFile(tmpFile, []byte(`{"type":"test"}`), 0644)

	resolved := &api.ResolvedAuth{
		Method: "test-method",
		Files: []api.FileMapping{
			{SourcePath: tmpFile, ContainerPath: ""},
		},
	}
	err := ValidateAuth(resolved)
	if err == nil {
		t.Fatal("expected error for empty container path")
	}
	if !strings.Contains(err.Error(), "no container path") {
		t.Errorf("error should mention missing container path: %v", err)
	}
}

func TestValidateAuth_EmptyEnvVarsAndFiles(t *testing.T) {
	// A valid resolved auth can have no env vars and no files (e.g. passthrough)
	resolved := &api.ResolvedAuth{
		Method: "passthrough",
	}
	if err := ValidateAuth(resolved); err != nil {
		t.Errorf("unexpected error for passthrough with no env/files: %v", err)
	}
}

func TestGatherAuthWithEnv_OverlayTakesPrecedence(t *testing.T) {
	// Set process env vars
	t.Setenv("GEMINI_API_KEY", "process-gemini")
	t.Setenv("ANTHROPIC_API_KEY", "process-anthropic")

	// Overlay should win over process env
	overlay := map[string]string{
		"GEMINI_API_KEY": "overlay-gemini",
	}

	auth := GatherAuthWithEnv(overlay)

	if auth.GeminiAPIKey != "overlay-gemini" {
		t.Errorf("GeminiAPIKey = %q, want %q (overlay should take precedence)", auth.GeminiAPIKey, "overlay-gemini")
	}
	// Non-overlaid key should fall back to process env
	if auth.AnthropicAPIKey != "process-anthropic" {
		t.Errorf("AnthropicAPIKey = %q, want %q (should fall back to process env)", auth.AnthropicAPIKey, "process-anthropic")
	}
}

func TestGatherAuthWithEnv_NilOverlay(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "process-gemini")
	t.Setenv("OPENAI_API_KEY", "process-openai")

	// nil overlay should behave identically to GatherAuth
	auth := GatherAuthWithEnv(nil)

	if auth.GeminiAPIKey != "process-gemini" {
		t.Errorf("GeminiAPIKey = %q, want %q", auth.GeminiAPIKey, "process-gemini")
	}
	if auth.OpenAIAPIKey != "process-openai" {
		t.Errorf("OpenAIAPIKey = %q, want %q", auth.OpenAIAPIKey, "process-openai")
	}
}

func TestRequiredAuthEnvKeys(t *testing.T) {
	tests := []struct {
		name     string
		harness  string
		authType string
		want     [][]string
	}{
		// Claude
		{"claude api-key", "claude", "api-key", [][]string{{"ANTHROPIC_API_KEY"}}},
		{"claude auth-file", "claude", "auth-file", nil},
		{"claude vertex-ai", "claude", "vertex-ai", [][]string{{"GOOGLE_CLOUD_PROJECT"}, {"GOOGLE_CLOUD_REGION"}}},

		// Gemini
		{"gemini api-key", "gemini", "api-key", [][]string{{"GEMINI_API_KEY", "GOOGLE_API_KEY"}}},
		{"gemini auth-file", "gemini", "auth-file", nil},
		{"gemini vertex-ai", "gemini", "vertex-ai", [][]string{{"GOOGLE_CLOUD_PROJECT"}}},

		// OpenCode
		{"opencode api-key", "opencode", "api-key", [][]string{{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}}},
		{"opencode auth-file", "opencode", "auth-file", nil},

		// Codex
		{"codex api-key", "codex", "api-key", [][]string{{"CODEX_API_KEY", "OPENAI_API_KEY"}}},
		{"codex auth-file", "codex", "auth-file", nil},

		// Generic
		{"generic api-key", "generic", "api-key", nil},
		{"generic vertex-ai", "generic", "vertex-ai", nil},

		// Unknown/empty
		{"empty harness", "", "api-key", nil},
		{"empty auth type", "claude", "", nil},
		{"both empty", "", "", nil},
		{"unknown harness", "unknown", "api-key", nil},
		{"unknown auth type", "claude", "unknown", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RequiredAuthEnvKeys(tt.harness, tt.authType)
			if tt.want == nil {
				if got != nil {
					t.Errorf("RequiredAuthEnvKeys(%q, %q) = %v, want nil", tt.harness, tt.authType, got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("RequiredAuthEnvKeys(%q, %q) returned %d groups, want %d", tt.harness, tt.authType, len(got), len(tt.want))
			}
			for i, group := range got {
				if len(group) != len(tt.want[i]) {
					t.Errorf("group %d: got %v, want %v", i, group, tt.want[i])
					continue
				}
				for j, key := range group {
					if key != tt.want[i][j] {
						t.Errorf("group %d key %d: got %q, want %q", i, j, key, tt.want[i][j])
					}
				}
			}
		})
	}
}

func TestGatherAuthWithEnv_EmptyOverlayValueFallsThrough(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "process-gemini")

	// Empty string in overlay should fall through to os.Getenv
	overlay := map[string]string{
		"GEMINI_API_KEY": "",
	}

	auth := GatherAuthWithEnv(overlay)

	if auth.GeminiAPIKey != "process-gemini" {
		t.Errorf("GeminiAPIKey = %q, want %q (empty overlay should fall through)", auth.GeminiAPIKey, "process-gemini")
	}
}

func TestGatherAuthWithEnv_OverlayProjectFallbacks(t *testing.T) {
	// Clear all project-related env vars from process
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCP_PROJECT", "")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")

	// Provide via overlay using the fallback key
	overlay := map[string]string{
		"GCP_PROJECT": "overlay-project",
	}

	auth := GatherAuthWithEnv(overlay)

	if auth.GoogleCloudProject != "overlay-project" {
		t.Errorf("GoogleCloudProject = %q, want %q (overlay fallback)", auth.GoogleCloudProject, "overlay-project")
	}
}

func TestGatherAuthWithEnv_OverlayAllKeys(t *testing.T) {
	// Clear all process env vars
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCP_PROJECT", "")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	t.Setenv("GOOGLE_CLOUD_REGION", "")
	t.Setenv("CLOUD_ML_REGION", "")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")

	overlay := map[string]string{
		"GEMINI_API_KEY":                 "ov-gemini",
		"GOOGLE_API_KEY":                 "ov-google",
		"ANTHROPIC_API_KEY":              "ov-anthropic",
		"OPENAI_API_KEY":                 "ov-openai",
		"CODEX_API_KEY":                  "ov-codex",
		"GOOGLE_CLOUD_PROJECT":           "ov-project",
		"GOOGLE_CLOUD_REGION":            "ov-region",
		"GOOGLE_APPLICATION_CREDENTIALS": "/ov/creds.json",
	}

	auth := GatherAuthWithEnv(overlay)

	if auth.GeminiAPIKey != "ov-gemini" {
		t.Errorf("GeminiAPIKey = %q, want %q", auth.GeminiAPIKey, "ov-gemini")
	}
	if auth.GoogleAPIKey != "ov-google" {
		t.Errorf("GoogleAPIKey = %q, want %q", auth.GoogleAPIKey, "ov-google")
	}
	if auth.AnthropicAPIKey != "ov-anthropic" {
		t.Errorf("AnthropicAPIKey = %q, want %q", auth.AnthropicAPIKey, "ov-anthropic")
	}
	if auth.OpenAIAPIKey != "ov-openai" {
		t.Errorf("OpenAIAPIKey = %q, want %q", auth.OpenAIAPIKey, "ov-openai")
	}
	if auth.CodexAPIKey != "ov-codex" {
		t.Errorf("CodexAPIKey = %q, want %q", auth.CodexAPIKey, "ov-codex")
	}
	if auth.GoogleCloudProject != "ov-project" {
		t.Errorf("GoogleCloudProject = %q, want %q", auth.GoogleCloudProject, "ov-project")
	}
	if auth.GoogleCloudRegion != "ov-region" {
		t.Errorf("GoogleCloudRegion = %q, want %q", auth.GoogleCloudRegion, "ov-region")
	}
	if auth.GoogleAppCredentials != "/ov/creds.json" {
		t.Errorf("GoogleAppCredentials = %q, want %q", auth.GoogleAppCredentials, "/ov/creds.json")
	}
}
