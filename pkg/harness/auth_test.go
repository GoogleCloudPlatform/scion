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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestGatherAuth_EnvVars(t *testing.T) {
	// Set up all env vars
	t.Setenv("GEMINI_API_KEY", "gemini-key")
	t.Setenv("GOOGLE_API_KEY", "google-key")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "claude-oauth-tok")
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
	if auth.ClaudeOAuthToken != "claude-oauth-tok" {
		t.Errorf("ClaudeOAuthToken = %q, want %q", auth.ClaudeOAuthToken, "claude-oauth-tok")
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
	_ = os.MkdirAll(filepath.Dir(adcPath), 0755)
	_ = os.WriteFile(adcPath, []byte(`{"type":"authorized_user"}`), 0644)

	oauthPath := filepath.Join(tmpHome, ".gemini", "oauth_creds.json")
	_ = os.MkdirAll(filepath.Dir(oauthPath), 0755)
	_ = os.WriteFile(oauthPath, []byte(`{"dummy":"oauth"}`), 0644)

	codexPath := filepath.Join(tmpHome, ".codex", "auth.json")
	_ = os.MkdirAll(filepath.Dir(codexPath), 0755)
	_ = os.WriteFile(codexPath, []byte(`{"dummy":"codex"}`), 0644)

	opencodePath := filepath.Join(tmpHome, ".local", "share", "opencode", "auth.json")
	_ = os.MkdirAll(filepath.Dir(opencodePath), 0755)
	_ = os.WriteFile(opencodePath, []byte(`{"dummy":"opencode"}`), 0644)

	claudeCredsPath := filepath.Join(tmpHome, ".claude", ".credentials.json")
	_ = os.MkdirAll(filepath.Dir(claudeCredsPath), 0755)
	_ = os.WriteFile(claudeCredsPath, []byte(`{"claudeAiOauth":{"accessToken":"rotating"}}`), 0644)

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
	if auth.ClaudeAuthFile != claudeCredsPath {
		t.Errorf("ClaudeAuthFile = %q, want %q", auth.ClaudeAuthFile, claudeCredsPath)
	}
	// The file must be treated opaquely — we must NOT have read/parsed
	// any content out of it into ClaudeOAuthToken. That field only comes
	// from the CLAUDE_CODE_OAUTH_TOKEN env var.
	if auth.ClaudeOAuthToken != "" {
		t.Errorf("ClaudeOAuthToken = %q, want empty (must not scrape from credentials file)", auth.ClaudeOAuthToken)
	}
}

func TestGatherAuth_EnvCredsTakePrecedenceOverFiles(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create the ADC file
	adcPath := filepath.Join(tmpHome, ".config", "gcloud", "application_default_credentials.json")
	_ = os.MkdirAll(filepath.Dir(adcPath), 0755)
	_ = os.WriteFile(adcPath, []byte(`{"type":"authorized_user"}`), 0644)

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
	if auth.ClaudeAuthFile != "" {
		t.Errorf("ClaudeAuthFile = %q, want empty", auth.ClaudeAuthFile)
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
	_ = os.WriteFile(tmpFile, []byte(`{"type":"test"}`), 0644)

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
	_ = os.WriteFile(tmpFile, []byte(`{"type":"test"}`), 0644)

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

	auth := GatherAuthWithEnv(overlay, true, nil)

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
	auth := GatherAuthWithEnv(nil, true, nil)

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
		{"claude oauth-token", "claude", "oauth-token", [][]string{{"CLAUDE_CODE_OAUTH_TOKEN"}}},
		{"claude auth-file", "claude", "auth-file", nil},
		{"claude vertex-ai", "claude", "vertex-ai", [][]string{{"GOOGLE_CLOUD_PROJECT"}, {"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"}}},

		// Gemini
		{"gemini api-key", "gemini", "api-key", [][]string{{"GEMINI_API_KEY", "GOOGLE_API_KEY"}}},
		{"gemini auth-file", "gemini", "auth-file", nil},
		{"gemini vertex-ai", "gemini", "vertex-ai", [][]string{{"GOOGLE_CLOUD_PROJECT"}, {"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"}}},

		// OpenCode
		{"opencode api-key", "opencode", "api-key", [][]string{{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}}},
		{"opencode auth-file", "opencode", "auth-file", nil},

		// Codex
		{"codex api-key", "codex", "api-key", [][]string{{"CODEX_API_KEY", "OPENAI_API_KEY"}}},
		{"codex auth-file", "codex", "auth-file", nil},

		// Generic
		{"generic api-key", "generic", "api-key", nil},
		{"generic vertex-ai", "generic", "vertex-ai", nil},

		// Empty authType defaults to api-key
		{"claude empty auth type", "claude", "", [][]string{{"ANTHROPIC_API_KEY"}}},
		{"gemini empty auth type", "gemini", "", [][]string{{"GEMINI_API_KEY", "GOOGLE_API_KEY"}}},
		{"opencode empty auth type", "opencode", "", [][]string{{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}}},
		{"codex empty auth type", "codex", "", [][]string{{"CODEX_API_KEY", "OPENAI_API_KEY"}}},

		// Unknown/empty
		{"empty harness", "", "api-key", nil},
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

func TestRequiredAuthSecrets(t *testing.T) {
	tests := []struct {
		name          string
		harness       string
		authType      string
		gcpSAAssigned bool
		wantNil       bool
		wantKey       string
		wantType      string
	}{
		{"claude vertex-ai", "claude", "vertex-ai", false, false, "gcloud-adc", "file"},
		{"gemini vertex-ai", "gemini", "vertex-ai", false, false, "gcloud-adc", "file"},
		{"opencode vertex-ai", "opencode", "vertex-ai", false, false, "gcloud-adc", "file"},
		{"codex vertex-ai", "codex", "vertex-ai", false, false, "gcloud-adc", "file"},
		{"claude api-key", "claude", "api-key", false, true, "", ""},
		{"gemini api-key", "gemini", "api-key", false, true, "", ""},
		{"claude empty auth type", "claude", "", false, true, "", ""},
		{"gemini empty auth type", "gemini", "", false, true, "", ""},
		{"generic vertex-ai", "generic", "vertex-ai", false, true, "", ""},
		{"unknown harness", "unknown", "vertex-ai", false, true, "", ""},
		{"empty harness", "", "vertex-ai", false, true, "", ""},
		{"both empty", "", "", false, true, "", ""},
		// GCP SA assigned — ADC not required
		{"claude vertex-ai with GCP SA", "claude", "vertex-ai", true, true, "", ""},
		{"gemini vertex-ai with GCP SA", "gemini", "vertex-ai", true, true, "", ""},
		{"opencode vertex-ai with GCP SA", "opencode", "vertex-ai", true, true, "", ""},
		{"codex vertex-ai with GCP SA", "codex", "vertex-ai", true, true, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RequiredAuthSecrets(tt.harness, tt.authType, tt.gcpSAAssigned)
			if tt.wantNil {
				if got != nil {
					t.Errorf("RequiredAuthSecrets(%q, %q, %t) = %v, want nil", tt.harness, tt.authType, tt.gcpSAAssigned, got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("RequiredAuthSecrets(%q, %q, %t) returned %d secrets, want 1", tt.harness, tt.authType, tt.gcpSAAssigned, len(got))
			}
			if got[0].Key != tt.wantKey {
				t.Errorf("Key = %q, want %q", got[0].Key, tt.wantKey)
			}
			if got[0].Type != tt.wantType {
				t.Errorf("Type = %q, want %q", got[0].Type, tt.wantType)
			}
			if got[0].Description == "" {
				t.Error("Description should not be empty")
			}
			// vertex-ai secrets should list GOOGLE_APPLICATION_CREDENTIALS as alternative
			if tt.authType == "vertex-ai" && !tt.gcpSAAssigned {
				if len(got[0].AlternativeEnvKeys) != 1 || got[0].AlternativeEnvKeys[0] != "GOOGLE_APPLICATION_CREDENTIALS" {
					t.Errorf("AlternativeEnvKeys = %v, want [GOOGLE_APPLICATION_CREDENTIALS]", got[0].AlternativeEnvKeys)
				}
			}
		})
	}
}

func TestDetectAuthTypeFromGCPIdentity(t *testing.T) {
	tests := []struct {
		name          string
		harness       string
		gcpSAAssigned bool
		wantType      string
	}{
		{"claude with GCP SA", "claude", true, "vertex-ai"},
		{"gemini with GCP SA", "gemini", true, "vertex-ai"},
		{"claude without GCP SA", "claude", false, ""},
		{"gemini without GCP SA", "gemini", false, ""},
		{"opencode with GCP SA", "opencode", true, ""},
		{"codex with GCP SA", "codex", true, ""},
		{"generic with GCP SA", "generic", true, ""},
		{"unknown with GCP SA", "unknown", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectAuthTypeFromGCPIdentity(tt.harness, tt.gcpSAAssigned)
			if got != tt.wantType {
				t.Errorf("DetectAuthTypeFromGCPIdentity(%q, %t) = %q, want %q", tt.harness, tt.gcpSAAssigned, got, tt.wantType)
			}
		})
	}
}

func TestDetectAuthTypeFromEnvVars(t *testing.T) {
	tests := []struct {
		name     string
		harness  string
		envKeys  map[string]struct{}
		wantType string
	}{
		{"claude with GAC", "claude", map[string]struct{}{"GOOGLE_APPLICATION_CREDENTIALS": {}}, "vertex-ai"},
		{"claude with GOOGLE_CLOUD_PROJECT", "claude", map[string]struct{}{"GOOGLE_CLOUD_PROJECT": {}}, "vertex-ai"},
		{"claude with CLAUDE_CODE_OAUTH_TOKEN", "claude", map[string]struct{}{"CLAUDE_CODE_OAUTH_TOKEN": {}}, "oauth-token"},
		{"claude prefers OAuth token over GAC", "claude", map[string]struct{}{"CLAUDE_CODE_OAUTH_TOKEN": {}, "GOOGLE_APPLICATION_CREDENTIALS": {}}, "oauth-token"},
		{"claude prefers OAuth token over GCP", "claude", map[string]struct{}{"CLAUDE_CODE_OAUTH_TOKEN": {}, "GOOGLE_CLOUD_PROJECT": {}}, "oauth-token"},
		{"gemini with GAC", "gemini", map[string]struct{}{"GOOGLE_APPLICATION_CREDENTIALS": {}}, "vertex-ai"},
		{"gemini with GOOGLE_CLOUD_PROJECT", "gemini", map[string]struct{}{"GOOGLE_CLOUD_PROJECT": {}}, "vertex-ai"},
		{"gemini with CLAUDE_CODE_OAUTH_TOKEN", "gemini", map[string]struct{}{"CLAUDE_CODE_OAUTH_TOKEN": {}}, ""},
		{"claude without GAC", "claude", map[string]struct{}{}, ""},
		{"gemini without GAC", "gemini", map[string]struct{}{}, ""},
		{"opencode with GAC", "opencode", map[string]struct{}{"GOOGLE_APPLICATION_CREDENTIALS": {}}, ""},
		{"opencode with GOOGLE_CLOUD_PROJECT", "opencode", map[string]struct{}{"GOOGLE_CLOUD_PROJECT": {}}, ""},
		{"codex with GAC", "codex", map[string]struct{}{"GOOGLE_APPLICATION_CREDENTIALS": {}}, ""},
		{"generic with GAC", "generic", map[string]struct{}{"GOOGLE_APPLICATION_CREDENTIALS": {}}, ""},
		{"claude with unrelated env", "claude", map[string]struct{}{"SOME_OTHER_VAR": {}}, ""},
		{"claude API key wins over GAC", "claude", map[string]struct{}{"ANTHROPIC_API_KEY": {}, "GOOGLE_APPLICATION_CREDENTIALS": {}}, ""},
		{"claude API key wins over GCP project", "claude", map[string]struct{}{"ANTHROPIC_API_KEY": {}, "GOOGLE_CLOUD_PROJECT": {}}, ""},
		{"claude API key alone", "claude", map[string]struct{}{"ANTHROPIC_API_KEY": {}}, ""},
		{"gemini API key wins over GAC", "gemini", map[string]struct{}{"GEMINI_API_KEY": {}, "GOOGLE_APPLICATION_CREDENTIALS": {}}, ""},
		{"gemini API key wins over GCP project", "gemini", map[string]struct{}{"GEMINI_API_KEY": {}, "GOOGLE_CLOUD_PROJECT": {}}, ""},
		{"gemini GOOGLE_API_KEY wins over GAC", "gemini", map[string]struct{}{"GOOGLE_API_KEY": {}, "GOOGLE_APPLICATION_CREDENTIALS": {}}, ""},
		{"gemini API key alone", "gemini", map[string]struct{}{"GEMINI_API_KEY": {}}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectAuthTypeFromEnvVars(tt.harness, tt.envKeys)
			if got != tt.wantType {
				t.Errorf("DetectAuthTypeFromEnvVars(%q, ...) = %q, want %q", tt.harness, got, tt.wantType)
			}
		})
	}
}

func TestDetectAuthTypeFromFileSecrets(t *testing.T) {
	tests := []struct {
		name     string
		harness  string
		secrets  map[string]struct{}
		wantType string
	}{
		{
			"gemini with GEMINI_OAUTH_CREDS",
			"gemini",
			map[string]struct{}{"GEMINI_OAUTH_CREDS": {}},
			"auth-file",
		},
		{
			"gemini with gcloud-adc",
			"gemini",
			map[string]struct{}{"gcloud-adc": {}},
			"vertex-ai",
		},
		{
			"gemini with both OAuth and ADC prefers OAuth",
			"gemini",
			map[string]struct{}{"GEMINI_OAUTH_CREDS": {}, "gcloud-adc": {}},
			"auth-file",
		},
		{
			"gemini with no file secrets",
			"gemini",
			map[string]struct{}{},
			"",
		},
		{
			"codex with CODEX_AUTH",
			"codex",
			map[string]struct{}{"CODEX_AUTH": {}},
			"auth-file",
		},
		{
			"opencode with OPENCODE_AUTH",
			"opencode",
			map[string]struct{}{"OPENCODE_AUTH": {}},
			"auth-file",
		},
		{
			"claude with gcloud-adc",
			"claude",
			map[string]struct{}{"gcloud-adc": {}},
			"vertex-ai",
		},
		{
			"claude with CLAUDE_AUTH",
			"claude",
			map[string]struct{}{"CLAUDE_AUTH": {}},
			"auth-file",
		},
		{
			"claude prefers CLAUDE_AUTH over gcloud-adc",
			"claude",
			map[string]struct{}{"CLAUDE_AUTH": {}, "gcloud-adc": {}},
			"auth-file",
		},
		{
			"claude with no file secrets",
			"claude",
			map[string]struct{}{},
			"",
		},
		{
			"unknown harness",
			"unknown",
			map[string]struct{}{"GEMINI_OAUTH_CREDS": {}},
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectAuthTypeFromFileSecrets(tt.harness, tt.secrets)
			if got != tt.wantType {
				t.Errorf("DetectAuthTypeFromFileSecrets(%q, ...) = %q, want %q", tt.harness, got, tt.wantType)
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

	auth := GatherAuthWithEnv(overlay, true, nil)

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

	auth := GatherAuthWithEnv(overlay, true, nil)

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

	auth := GatherAuthWithEnv(overlay, true, nil)

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

func TestGatherAuthWithEnv_GCPMetadataMode(t *testing.T) {
	t.Setenv("SCION_METADATA_MODE", "")

	// From overlay
	overlay := map[string]string{
		"SCION_METADATA_MODE": "assign",
	}
	auth := GatherAuthWithEnv(overlay, true, nil)
	if auth.GCPMetadataMode != "assign" {
		t.Errorf("GCPMetadataMode = %q, want %q", auth.GCPMetadataMode, "assign")
	}

	// From process env
	t.Setenv("SCION_METADATA_MODE", "block")
	auth2 := GatherAuthWithEnv(nil, true, nil)
	if auth2.GCPMetadataMode != "block" {
		t.Errorf("GCPMetadataMode = %q, want %q", auth2.GCPMetadataMode, "block")
	}
}

func TestOverlaySettings_ReadsScionAgentJSON(t *testing.T) {
	tmpDir := t.TempDir()
	agentHome := filepath.Join(tmpDir, "home")
	_ = os.MkdirAll(agentHome, 0755)

	// Write scion-agent.json with a universal auth type
	scionAgentPath := filepath.Join(tmpDir, "scion-agent.json")
	_ = os.WriteFile(scionAgentPath, []byte(`{"auth_selectedType": "auth-file"}`), 0644)

	auth := api.AuthConfig{}
	h := New("gemini")
	OverlaySettings(&auth, h, tmpDir)

	if auth.SelectedType != "auth-file" {
		t.Errorf("SelectedType = %q, want %q", auth.SelectedType, "auth-file")
	}
}

func TestOverlaySettings_IgnoresHostGeminiSettings(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Write a host ~/.gemini/settings.json with a Gemini-internal auth type
	geminiDir := filepath.Join(tmpHome, ".gemini")
	_ = os.MkdirAll(geminiDir, 0755)
	_ = os.WriteFile(filepath.Join(geminiDir, "settings.json"),
		[]byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0644)

	// Agent dir with no scion-agent.json (or one without auth_selectedType)
	tmpDir := t.TempDir()
	agentHome := filepath.Join(tmpDir, "home")
	_ = os.MkdirAll(agentHome, 0755)

	auth := api.AuthConfig{}
	h := New("gemini")
	OverlaySettings(&auth, h, tmpDir)

	// Should NOT pick up "oauth-personal" from host Gemini settings
	if auth.SelectedType != "" {
		t.Errorf("SelectedType = %q, want empty (should not read host Gemini settings)", auth.SelectedType)
	}
}

func TestOverlaySettings_NoScionAgentJSON(t *testing.T) {
	tmpDir := t.TempDir()
	agentHome := filepath.Join(tmpDir, "home")
	_ = os.MkdirAll(agentHome, 0755)

	// No scion-agent.json exists
	auth := api.AuthConfig{}
	h := New("gemini")
	OverlaySettings(&auth, h, tmpDir)

	if auth.SelectedType != "" {
		t.Errorf("SelectedType = %q, want empty", auth.SelectedType)
	}
}

func TestGatherAuthWithEnv_BrokerMode(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Set broker-local env vars that should NOT leak into broker mode
	t.Setenv("GEMINI_API_KEY", "broker-gemini")
	t.Setenv("ANTHROPIC_API_KEY", "broker-anthropic")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")

	// Create credential files on the broker filesystem
	adcPath := filepath.Join(tmpHome, ".config", "gcloud", "application_default_credentials.json")
	_ = os.MkdirAll(filepath.Dir(adcPath), 0755)
	_ = os.WriteFile(adcPath, []byte(`{"type":"authorized_user"}`), 0644)

	oauthPath := filepath.Join(tmpHome, ".gemini", "oauth_creds.json")
	_ = os.MkdirAll(filepath.Dir(oauthPath), 0755)
	_ = os.WriteFile(oauthPath, []byte(`{"dummy":"oauth"}`), 0644)

	// Call with localSources=false and an overlay that provides one key
	overlay := map[string]string{
		"ANTHROPIC_API_KEY": "hub-anthropic",
	}
	auth := GatherAuthWithEnv(overlay, false, nil)

	// Overlay key should be present
	if auth.AnthropicAPIKey != "hub-anthropic" {
		t.Errorf("AnthropicAPIKey = %q, want %q (from overlay)", auth.AnthropicAPIKey, "hub-anthropic")
	}

	// Broker env should NOT leak through
	if auth.GeminiAPIKey != "" {
		t.Errorf("GeminiAPIKey = %q, want empty (broker env should not leak)", auth.GeminiAPIKey)
	}

	// Filesystem creds should NOT be discovered
	if auth.GoogleAppCredentials != "" {
		t.Errorf("GoogleAppCredentials = %q, want empty (filesystem should not be scanned)", auth.GoogleAppCredentials)
	}
	if auth.OAuthCreds != "" {
		t.Errorf("OAuthCreds = %q, want empty (filesystem should not be scanned)", auth.OAuthCreds)
	}
	if auth.ClaudeAuthFile != "" {
		t.Errorf("ClaudeAuthFile = %q, want empty (filesystem should not be scanned)", auth.ClaudeAuthFile)
	}
}

func TestOverlayFileSecrets(t *testing.T) {
	tests := []struct {
		name    string
		secrets []api.ResolvedSecret
		check   func(t *testing.T, auth api.AuthConfig)
	}{
		{
			name: "ADC by name",
			secrets: []api.ResolvedSecret{
				{Name: "gcloud-adc", Type: "file", Target: "/home/gemini/.config/gcloud/application_default_credentials.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.GoogleAppCredentials != "/home/gemini/.config/gcloud/application_default_credentials.json" {
					t.Errorf("GoogleAppCredentials = %q, want ADC path", auth.GoogleAppCredentials)
				}
			},
		},
		{
			name: "ADC by target suffix",
			secrets: []api.ResolvedSecret{
				{Name: "my-adc", Type: "file", Target: "/home/gemini/.config/gcloud/application_default_credentials.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.GoogleAppCredentials == "" {
					t.Error("GoogleAppCredentials should be set from target suffix match")
				}
			},
		},
		{
			name: "OAuth by name",
			secrets: []api.ResolvedSecret{
				{Name: "GEMINI_OAUTH_CREDS", Type: "file", Target: "/home/gemini/.gemini/oauth_creds.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.OAuthCreds != "/home/gemini/.gemini/oauth_creds.json" {
					t.Errorf("OAuthCreds = %q, want oauth path", auth.OAuthCreds)
				}
			},
		},
		{
			name: "Codex by name",
			secrets: []api.ResolvedSecret{
				{Name: "CODEX_AUTH", Type: "file", Target: "/home/gemini/.codex/auth.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.CodexAuthFile != "/home/gemini/.codex/auth.json" {
					t.Errorf("CodexAuthFile = %q, want codex path", auth.CodexAuthFile)
				}
			},
		},
		{
			name: "OpenCode by target suffix",
			secrets: []api.ResolvedSecret{
				{Name: "my-opencode", Type: "file", Target: "/home/gemini/.local/share/opencode/auth.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.OpenCodeAuthFile != "/home/gemini/.local/share/opencode/auth.json" {
					t.Errorf("OpenCodeAuthFile = %q, want opencode path", auth.OpenCodeAuthFile)
				}
			},
		},
		{
			name: "Claude credentials by name",
			secrets: []api.ResolvedSecret{
				{Name: "CLAUDE_AUTH", Type: "file", Target: "/home/agent/.claude/.credentials.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.ClaudeAuthFile != "/home/agent/.claude/.credentials.json" {
					t.Errorf("ClaudeAuthFile = %q, want credentials path", auth.ClaudeAuthFile)
				}
			},
		},
		{
			name: "Claude credentials by target suffix",
			secrets: []api.ResolvedSecret{
				{Name: "my-claude-creds", Type: "file", Target: "/home/agent/.claude/.credentials.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.ClaudeAuthFile == "" {
					t.Error("ClaudeAuthFile should be set from target suffix match")
				}
			},
		},
		{
			name: "non-file secrets are skipped",
			secrets: []api.ResolvedSecret{
				{Name: "gcloud-adc", Type: "environment", Target: "gcloud-adc", Value: "/some/path"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.GoogleAppCredentials != "" {
					t.Errorf("GoogleAppCredentials = %q, want empty (env-type secret should be skipped)", auth.GoogleAppCredentials)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := api.AuthConfig{}
			OverlayFileSecrets(&auth, tt.secrets)
			tt.check(t, auth)
		})
	}
}

func TestOverlayFileSecretsFromConfig(t *testing.T) {
	claudeAuthMeta := &config.HarnessAuthMetadata{
		DefaultType: "api-key",
		Types: map[string]config.HarnessAuthTypeMetadata{
			"auth-file": {
				RequiredFiles: []config.HarnessAuthFileRequirement{
					{Name: "CLAUDE_AUTH", Type: "file", TargetSuffix: "/.claude/.credentials.json", Field: "ClaudeAuthFile"},
				},
			},
			"vertex-ai": {
				RequiredFiles: []config.HarnessAuthFileRequirement{
					{Name: "gcloud-adc", Type: "file", TargetSuffix: "", Field: "GoogleAppCredentials", Required: true},
				},
			},
		},
	}

	tests := []struct {
		name    string
		meta    *config.HarnessAuthMetadata
		secrets []api.ResolvedSecret
		check   func(t *testing.T, auth api.AuthConfig)
	}{
		{
			name: "config-driven field mapping for CLAUDE_AUTH",
			meta: claudeAuthMeta,
			secrets: []api.ResolvedSecret{
				{Name: "CLAUDE_AUTH", Type: "file", Target: "/home/agent/.claude/.credentials.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.ClaudeAuthFile != "/home/agent/.claude/.credentials.json" {
					t.Errorf("ClaudeAuthFile = %q, want credentials path", auth.ClaudeAuthFile)
				}
			},
		},
		{
			name: "fallback to target suffix for unknown secret name",
			meta: claudeAuthMeta,
			secrets: []api.ResolvedSecret{
				{Name: "my-custom-claude-creds", Type: "file", Target: "/home/agent/.claude/.credentials.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.ClaudeAuthFile != "/home/agent/.claude/.credentials.json" {
					t.Errorf("ClaudeAuthFile = %q, want credentials path from suffix fallback", auth.ClaudeAuthFile)
				}
			},
		},
		{
			name: "config-driven matches hardcoded behavior",
			meta: claudeAuthMeta,
			secrets: []api.ResolvedSecret{
				{Name: "CLAUDE_AUTH", Type: "file", Target: "/home/agent/.claude/.credentials.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				hardcoded := api.AuthConfig{}
				OverlayFileSecrets(&hardcoded, []api.ResolvedSecret{
					{Name: "CLAUDE_AUTH", Type: "file", Target: "/home/agent/.claude/.credentials.json"},
				})
				if auth.ClaudeAuthFile != hardcoded.ClaudeAuthFile {
					t.Errorf("config-driven ClaudeAuthFile = %q, hardcoded = %q", auth.ClaudeAuthFile, hardcoded.ClaudeAuthFile)
				}
			},
		},
		{
			name: "non-file secrets are skipped",
			meta: claudeAuthMeta,
			secrets: []api.ResolvedSecret{
				{Name: "CLAUDE_AUTH", Type: "environment", Target: "CLAUDE_AUTH", Value: "some-value"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.ClaudeAuthFile != "" {
					t.Errorf("ClaudeAuthFile = %q, want empty (env-type should be skipped)", auth.ClaudeAuthFile)
				}
			},
		},
		{
			name: "nil auth metadata falls back to suffix matching",
			meta: nil,
			secrets: []api.ResolvedSecret{
				{Name: "CLAUDE_AUTH", Type: "file", Target: "/home/agent/.claude/.credentials.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.ClaudeAuthFile != "/home/agent/.claude/.credentials.json" {
					t.Errorf("ClaudeAuthFile = %q, want credentials path from suffix fallback", auth.ClaudeAuthFile)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := api.AuthConfig{}
			OverlayFileSecretsFromConfig(&auth, tt.secrets, tt.meta)
			tt.check(t, auth)
		})
	}
}

func TestStageCaptureAuthAssets(t *testing.T) {
	authMeta := &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"auth-file": {
				RequiredFiles: []config.HarnessAuthFileRequirement{
					{Name: "CLAUDE_AUTH", Type: "file", TargetSuffix: "/.claude/.credentials.json", Field: "ClaudeAuthFile"},
				},
			},
			"vertex-ai": {
				RequiredFiles: []config.HarnessAuthFileRequirement{
					{Name: "gcloud-adc", Type: "file", TargetSuffix: "", Field: "GoogleAppCredentials"},
				},
			},
		},
	}

	t.Run("stages capture-auth-config.json from auth metadata", func(t *testing.T) {
		agentHome := t.TempDir()
		configDir := t.TempDir()

		if err := os.WriteFile(filepath.Join(configDir, "capture_auth.py"), []byte("#!/usr/bin/env python3\n"), 0644); err != nil {
			t.Fatal(err)
		}

		if err := StageCaptureAuthAssets(agentHome, configDir, authMeta); err != nil {
			t.Fatalf("StageCaptureAuthAssets failed: %v", err)
		}

		scriptPath := filepath.Join(agentHome, ".scion", "harness", "capture_auth.py")
		if _, err := os.Stat(scriptPath); err != nil {
			t.Errorf("capture_auth.py not staged: %v", err)
		}

		configPath := filepath.Join(agentHome, ".scion", "harness", "inputs", "capture-auth-config.json")
		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("capture-auth-config.json not staged: %v", err)
		}

		var payload map[string]interface{}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}

		creds, ok := payload["credentials"].([]interface{})
		if !ok {
			t.Fatal("credentials field missing or not an array")
		}

		// Only CLAUDE_AUTH has a TargetSuffix, so only it should appear
		if len(creds) != 1 {
			t.Fatalf("expected 1 credential entry, got %d", len(creds))
		}

		entry := creds[0].(map[string]interface{})
		if entry["key"] != "CLAUDE_AUTH" {
			t.Errorf("key = %q, want CLAUDE_AUTH", entry["key"])
		}
		if entry["source"] != "~/.claude/.credentials.json" {
			t.Errorf("source = %q, want ~/.claude/.credentials.json", entry["source"])
		}
	})

	t.Run("no-op with nil auth metadata", func(t *testing.T) {
		agentHome := t.TempDir()
		configDir := t.TempDir()

		if err := StageCaptureAuthAssets(agentHome, configDir, nil); err != nil {
			t.Fatalf("StageCaptureAuthAssets failed: %v", err)
		}

		configPath := filepath.Join(agentHome, ".scion", "harness", "inputs", "capture-auth-config.json")
		if _, err := os.Stat(configPath); !os.IsNotExist(err) {
			t.Error("expected no capture-auth-config.json with nil auth metadata")
		}
	})

	t.Run("script is executable", func(t *testing.T) {
		agentHome := t.TempDir()
		configDir := t.TempDir()

		if err := os.WriteFile(filepath.Join(configDir, "capture_auth.py"), []byte("#!/usr/bin/env python3\n"), 0644); err != nil {
			t.Fatal(err)
		}

		if err := StageCaptureAuthAssets(agentHome, configDir, authMeta); err != nil {
			t.Fatal(err)
		}

		scriptPath := filepath.Join(agentHome, ".scion", "harness", "capture_auth.py")
		info, err := os.Stat(scriptPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0111 == 0 {
			t.Error("capture_auth.py should be executable")
		}
	})
}

func TestGatherAuthWithEnv_ConfigDrivenEnvVars(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "ghp_test123")
	t.Setenv("GH_TOKEN", "gh_test456")

	authMeta := &config.HarnessAuthMetadata{
		DefaultType: "api-key",
		Types: map[string]config.HarnessAuthTypeMetadata{
			"api-key": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "SCION_TEST_UNSET_TOKEN"}},
				},
			},
		},
	}

	auth := GatherAuthWithEnv(nil, true, authMeta)

	if auth.EnvVars == nil {
		t.Fatal("EnvVars should not be nil when config metadata declares env vars")
	}
	if auth.EnvVars["COPILOT_GITHUB_TOKEN"] != "ghp_test123" {
		t.Errorf("COPILOT_GITHUB_TOKEN = %q, want %q", auth.EnvVars["COPILOT_GITHUB_TOKEN"], "ghp_test123")
	}
	if auth.EnvVars["GH_TOKEN"] != "gh_test456" {
		t.Errorf("GH_TOKEN = %q, want %q", auth.EnvVars["GH_TOKEN"], "gh_test456")
	}
	if _, ok := auth.EnvVars["SCION_TEST_UNSET_TOKEN"]; ok {
		t.Error("SCION_TEST_UNSET_TOKEN should not be in EnvVars when not set in environment")
	}
}

func TestGatherAuthWithEnv_ConfigDrivenEnvVarsFromOverlay(t *testing.T) {
	overlay := map[string]string{
		"COPILOT_GITHUB_TOKEN": "overlay-token",
	}

	authMeta := &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"api-key": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}},
				},
			},
		},
	}

	auth := GatherAuthWithEnv(overlay, true, authMeta)

	if auth.EnvVars == nil {
		t.Fatal("EnvVars should not be nil")
	}
	if auth.EnvVars["COPILOT_GITHUB_TOKEN"] != "overlay-token" {
		t.Errorf("COPILOT_GITHUB_TOKEN = %q, want %q", auth.EnvVars["COPILOT_GITHUB_TOKEN"], "overlay-token")
	}
}

func TestGatherAuthWithEnv_NilAuthMetaNoEnvVars(t *testing.T) {
	auth := GatherAuthWithEnv(nil, true, nil)
	if auth.EnvVars != nil {
		t.Errorf("EnvVars should be nil when authMeta is nil, got %v", auth.EnvVars)
	}
}

func TestGatherAuthWithEnv_EmptyAuthMetaNoEnvVars(t *testing.T) {
	authMeta := &config.HarnessAuthMetadata{}
	auth := GatherAuthWithEnv(nil, true, authMeta)
	if auth.EnvVars != nil {
		t.Errorf("EnvVars should be nil when authMeta has no types, got %v", auth.EnvVars)
	}
}

func TestGatherAuthWithEnv_ConfigDrivenMultipleAuthTypes(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "ghp_test")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "my-project")

	authMeta := &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"api-key": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN"}},
				},
			},
			"vertex-ai": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"GOOGLE_CLOUD_PROJECT"}},
					{AnyOf: []string{"GOOGLE_CLOUD_REGION"}},
				},
			},
		},
	}

	auth := GatherAuthWithEnv(nil, true, authMeta)

	if auth.EnvVars["COPILOT_GITHUB_TOKEN"] != "ghp_test" {
		t.Errorf("COPILOT_GITHUB_TOKEN = %q, want %q", auth.EnvVars["COPILOT_GITHUB_TOKEN"], "ghp_test")
	}
	if auth.EnvVars["GOOGLE_CLOUD_PROJECT"] != "my-project" {
		t.Errorf("GOOGLE_CLOUD_PROJECT = %q, want %q", auth.EnvVars["GOOGLE_CLOUD_PROJECT"], "my-project")
	}
	if _, ok := auth.EnvVars["GOOGLE_CLOUD_REGION"]; ok {
		t.Error("GOOGLE_CLOUD_REGION should not be in EnvVars when not set")
	}
}

func TestGatherAuthWithEnv_BrokerModeConfigDriven(t *testing.T) {
	overlay := map[string]string{
		"COPILOT_GITHUB_TOKEN": "broker-token",
	}

	authMeta := &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"api-key": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"COPILOT_GITHUB_TOKEN"}},
				},
			},
		},
	}

	// In broker mode (localSources=false), env vars come only from overlay
	t.Setenv("COPILOT_GITHUB_TOKEN", "should-not-see-this")
	auth := GatherAuthWithEnv(overlay, false, authMeta)

	if auth.EnvVars["COPILOT_GITHUB_TOKEN"] != "broker-token" {
		t.Errorf("COPILOT_GITHUB_TOKEN = %q, want overlay value %q", auth.EnvVars["COPILOT_GITHUB_TOKEN"], "broker-token")
	}
}
