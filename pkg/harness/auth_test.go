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

// testMultiHarnessAuthMeta returns auth metadata that exercises multiple
// provider env-var groups (Gemini, OpenAI, Codex, etc.) — used by tests
// that verify config-driven env-var gathering across provider types.
func testMultiHarnessAuthMeta() *config.HarnessAuthMetadata {
	return &config.HarnessAuthMetadata{
		DefaultType: "api-key",
		Types: map[string]config.HarnessAuthTypeMetadata{
			"api-key": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
					{AnyOf: []string{"ANTHROPIC_API_KEY"}},
					{AnyOf: []string{"CLAUDE_CODE_OAUTH_TOKEN"}},
					{AnyOf: []string{"OPENAI_API_KEY"}},
					{AnyOf: []string{"CODEX_API_KEY"}},
				},
			},
		},
	}
}

// testFileDiscoveryAuthMeta returns auth metadata with file credential
// entries for OAuth, Codex, OpenCode, and Claude — used by file discovery
// tests.
func testFileDiscoveryAuthMeta() *config.HarnessAuthMetadata {
	return &config.HarnessAuthMetadata{
		DefaultType: "api-key",
		Types: map[string]config.HarnessAuthTypeMetadata{
			"auth-file": {
				RequiredFiles: []config.HarnessAuthFileRequirement{
					{Name: "GEMINI_OAUTH_CREDS", Type: "file", TargetSuffix: "/.gemini/oauth_creds.json", Field: "OAuthCreds"},
					{Name: "CODEX_AUTH", Type: "file", TargetSuffix: "/.codex/auth.json", Field: "CodexAuthFile"},
					{Name: "OPENCODE_AUTH", Type: "file", TargetSuffix: "/.local/share/opencode/auth.json", Field: "OpenCodeAuthFile"},
					{Name: "CLAUDE_AUTH", Type: "file", TargetSuffix: "/.claude/.credentials.json", Field: "ClaudeAuthFile"},
				},
			},
		},
	}
}

func TestGatherAuth_GCPSharedFields(t *testing.T) {
	// GatherAuth() with nil authMeta only populates GCP shared fields.
	t.Setenv("GOOGLE_CLOUD_PROJECT", "my-project")
	t.Setenv("GOOGLE_CLOUD_REGION", "us-central1")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/path/to/creds.json")

	auth := GatherAuth()

	if auth.GoogleCloudProject != "my-project" {
		t.Errorf("GoogleCloudProject = %q, want %q", auth.GoogleCloudProject, "my-project")
	}
	if auth.GoogleCloudRegion != "us-central1" {
		t.Errorf("GoogleCloudRegion = %q, want %q", auth.GoogleCloudRegion, "us-central1")
	}
	if auth.GoogleAppCredentials != "/path/to/creds.json" {
		t.Errorf("GoogleAppCredentials = %q, want %q", auth.GoogleAppCredentials, "/path/to/creds.json")
	}

	// With nil authMeta, EnvVars and Files are not populated.
	if auth.EnvVars != nil {
		t.Errorf("EnvVars should be nil with nil authMeta, got %v", auth.EnvVars)
	}
	if auth.Files != nil {
		t.Errorf("Files should be nil with nil authMeta, got %v", auth.Files)
	}
}

func TestGatherAuthWithEnv_ConfigDrivenEnvVarsFromProcess(t *testing.T) {
	// With non-nil authMeta, per-provider env vars are gathered into EnvVars.
	t.Setenv("GEMINI_API_KEY", "gemini-key")
	t.Setenv("GOOGLE_API_KEY", "google-key")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "claude-oauth-tok")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("CODEX_API_KEY", "codex-key")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "my-project")
	t.Setenv("GOOGLE_CLOUD_REGION", "us-central1")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/path/to/creds.json")

	meta := testMultiHarnessAuthMeta()
	auth := GatherAuthWithEnv(nil, true, meta)

	// GCP shared fields are always populated directly.
	if auth.GoogleCloudProject != "my-project" {
		t.Errorf("GoogleCloudProject = %q, want %q", auth.GoogleCloudProject, "my-project")
	}
	if auth.GoogleCloudRegion != "us-central1" {
		t.Errorf("GoogleCloudRegion = %q, want %q", auth.GoogleCloudRegion, "us-central1")
	}
	if auth.GoogleAppCredentials != "/path/to/creds.json" {
		t.Errorf("GoogleAppCredentials = %q, want %q", auth.GoogleAppCredentials, "/path/to/creds.json")
	}

	// Per-provider env vars are now in EnvVars.
	if auth.EnvVars == nil {
		t.Fatal("EnvVars should not be nil when authMeta is provided")
	}
	checks := map[string]string{
		"GEMINI_API_KEY":          "gemini-key",
		"GOOGLE_API_KEY":          "google-key",
		"ANTHROPIC_API_KEY":       "anthropic-key",
		"CLAUDE_CODE_OAUTH_TOKEN": "claude-oauth-tok",
		"OPENAI_API_KEY":          "openai-key",
		"CODEX_API_KEY":           "codex-key",
	}
	for key, want := range checks {
		if got := auth.EnvVars[key]; got != want {
			t.Errorf("EnvVars[%q] = %q, want %q", key, got, want)
		}
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

	// Use GatherAuthWithEnv with file-discovery authMeta so that per-harness
	// file credentials flow through auth.Files.
	meta := testFileDiscoveryAuthMeta()
	auth := GatherAuthWithEnv(nil, true, meta)

	// ADC is still a first-class GCP shared field.
	if auth.GoogleAppCredentials != adcPath {
		t.Errorf("GoogleAppCredentials = %q, want %q", auth.GoogleAppCredentials, adcPath)
	}

	// Per-harness file credentials are now in auth.Files.
	if auth.Files == nil {
		t.Fatal("Files should not be nil when authMeta has file requirements")
	}
	if auth.Files["OAuthCreds"] != oauthPath {
		t.Errorf("Files[OAuthCreds] = %q, want %q", auth.Files["OAuthCreds"], oauthPath)
	}
	if auth.Files["CodexAuthFile"] != codexPath {
		t.Errorf("Files[CodexAuthFile] = %q, want %q", auth.Files["CodexAuthFile"], codexPath)
	}
	if auth.Files["OpenCodeAuthFile"] != opencodePath {
		t.Errorf("Files[OpenCodeAuthFile] = %q, want %q", auth.Files["OpenCodeAuthFile"], opencodePath)
	}
	if auth.Files["ClaudeAuthFile"] != claudeCredsPath {
		t.Errorf("Files[ClaudeAuthFile] = %q, want %q", auth.Files["ClaudeAuthFile"], claudeCredsPath)
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

	// With nil authMeta, no file scanning occurs for per-harness files.
	auth := GatherAuth()

	if auth.GoogleAppCredentials != "" {
		t.Errorf("GoogleAppCredentials = %q, want empty", auth.GoogleAppCredentials)
	}

	// With authMeta but no files on disk, Files should be nil.
	meta := testFileDiscoveryAuthMeta()
	auth2 := GatherAuthWithEnv(nil, true, meta)
	if auth2.Files != nil {
		t.Errorf("Files = %v, want nil (no files on disk)", auth2.Files)
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
	t.Setenv("ANTHROPIC_API_KEY", "process-anthropic")
	t.Setenv("GEMINI_API_KEY", "process-gemini")

	// Overlay should win over process env
	overlay := map[string]string{
		"GEMINI_API_KEY": "overlay-gemini",
	}

	meta := testMultiHarnessAuthMeta()
	auth := GatherAuthWithEnv(overlay, true, meta)

	if auth.EnvVars["GEMINI_API_KEY"] != "overlay-gemini" {
		t.Errorf("EnvVars[GEMINI_API_KEY] = %q, want %q (overlay should take precedence)", auth.EnvVars["GEMINI_API_KEY"], "overlay-gemini")
	}
	// Non-overlaid key should fall back to process env
	if auth.EnvVars["ANTHROPIC_API_KEY"] != "process-anthropic" {
		t.Errorf("EnvVars[ANTHROPIC_API_KEY] = %q, want %q (should fall back to process env)", auth.EnvVars["ANTHROPIC_API_KEY"], "process-anthropic")
	}
}

func TestGatherAuthWithEnv_NilOverlay(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "process-gemini")
	t.Setenv("OPENAI_API_KEY", "process-openai")

	meta := testMultiHarnessAuthMeta()
	// nil overlay should behave identically to GatherAuth (with authMeta)
	auth := GatherAuthWithEnv(nil, true, meta)

	if auth.EnvVars["GEMINI_API_KEY"] != "process-gemini" {
		t.Errorf("EnvVars[GEMINI_API_KEY] = %q, want %q", auth.EnvVars["GEMINI_API_KEY"], "process-gemini")
	}
	if auth.EnvVars["OPENAI_API_KEY"] != "process-openai" {
		t.Errorf("EnvVars[OPENAI_API_KEY] = %q, want %q", auth.EnvVars["OPENAI_API_KEY"], "process-openai")
	}
}

func TestGatherAuthWithEnv_EmptyOverlayValueFallsThrough(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "process-gemini")

	// Empty string in overlay should fall through to os.Getenv
	overlay := map[string]string{
		"GEMINI_API_KEY": "",
	}

	meta := testMultiHarnessAuthMeta()
	auth := GatherAuthWithEnv(overlay, true, meta)

	if auth.EnvVars["GEMINI_API_KEY"] != "process-gemini" {
		t.Errorf("EnvVars[GEMINI_API_KEY] = %q, want %q (empty overlay should fall through)", auth.EnvVars["GEMINI_API_KEY"], "process-gemini")
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

	meta := testMultiHarnessAuthMeta()
	auth := GatherAuthWithEnv(overlay, true, meta)

	// GCP shared fields
	if auth.GoogleCloudProject != "ov-project" {
		t.Errorf("GoogleCloudProject = %q, want %q", auth.GoogleCloudProject, "ov-project")
	}
	if auth.GoogleCloudRegion != "ov-region" {
		t.Errorf("GoogleCloudRegion = %q, want %q", auth.GoogleCloudRegion, "ov-region")
	}
	if auth.GoogleAppCredentials != "/ov/creds.json" {
		t.Errorf("GoogleAppCredentials = %q, want %q", auth.GoogleAppCredentials, "/ov/creds.json")
	}

	// Per-provider env vars via EnvVars
	envChecks := map[string]string{
		"GEMINI_API_KEY":    "ov-gemini",
		"GOOGLE_API_KEY":    "ov-google",
		"ANTHROPIC_API_KEY": "ov-anthropic",
		"OPENAI_API_KEY":    "ov-openai",
		"CODEX_API_KEY":     "ov-codex",
	}
	if auth.EnvVars == nil {
		t.Fatal("EnvVars should not be nil")
	}
	for key, want := range envChecks {
		if got := auth.EnvVars[key]; got != want {
			t.Errorf("EnvVars[%q] = %q, want %q", key, got, want)
		}
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

// TestOverlaySettings_RejectsHarnessImplementationName verifies that
// OverlaySettings does NOT set auth.SelectedType when scion-agent.json
// contains a harness implementation name (e.g. "container-script"). This
// guards against data corruption where the harness implementation name
// leaks into AuthSelectedType. See issue #723.
func TestOverlaySettings_RejectsHarnessImplementationName(t *testing.T) {
	for _, badValue := range []string{"container-script", "generic", "builtin", "passthrough"} {
		t.Run(badValue, func(t *testing.T) {
			tmpDir := t.TempDir()
			scionAgentPath := filepath.Join(tmpDir, "scion-agent.json")
			_ = os.WriteFile(scionAgentPath,
				[]byte(`{"auth_selectedType": "`+badValue+`"}`), 0644)

			auth := api.AuthConfig{}
			h := New("gemini")
			OverlaySettings(&auth, h, tmpDir)

			if auth.SelectedType != "" {
				t.Errorf("SelectedType = %q, want empty (harness implementation name should be rejected)", auth.SelectedType)
			}
		})
	}
}

// TestOverlaySettings_AcceptsValidAuthType verifies that OverlaySettings still
// accepts legitimate auth types like "vertex-ai", "api-key", "auth-file".
func TestOverlaySettings_AcceptsValidAuthType(t *testing.T) {
	for _, validType := range []string{"vertex-ai", "api-key", "auth-file", "adc"} {
		t.Run(validType, func(t *testing.T) {
			tmpDir := t.TempDir()
			scionAgentPath := filepath.Join(tmpDir, "scion-agent.json")
			_ = os.WriteFile(scionAgentPath,
				[]byte(`{"auth_selectedType": "`+validType+`"}`), 0644)

			auth := api.AuthConfig{}
			h := New("gemini")
			OverlaySettings(&auth, h, tmpDir)

			if auth.SelectedType != validType {
				t.Errorf("SelectedType = %q, want %q", auth.SelectedType, validType)
			}
		})
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

	// Call with localSources=false and an overlay that provides one key
	overlay := map[string]string{
		"ANTHROPIC_API_KEY": "hub-anthropic",
	}
	meta := testMultiHarnessAuthMeta()
	auth := GatherAuthWithEnv(overlay, false, meta)

	// Overlay key should be present in EnvVars
	if auth.EnvVars == nil {
		t.Fatal("EnvVars should not be nil")
	}
	if auth.EnvVars["ANTHROPIC_API_KEY"] != "hub-anthropic" {
		t.Errorf("EnvVars[ANTHROPIC_API_KEY] = %q, want %q (from overlay)", auth.EnvVars["ANTHROPIC_API_KEY"], "hub-anthropic")
	}

	// Broker env should NOT leak through
	if _, ok := auth.EnvVars["GEMINI_API_KEY"]; ok {
		t.Errorf("EnvVars[GEMINI_API_KEY] should not be set (broker env should not leak)")
	}

	// Filesystem creds should NOT be discovered
	if auth.GoogleAppCredentials != "" {
		t.Errorf("GoogleAppCredentials = %q, want empty (filesystem should not be scanned)", auth.GoogleAppCredentials)
	}
	if auth.Files != nil {
		t.Errorf("Files should be nil in broker mode (filesystem should not be scanned)")
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
			name: "OAuth by target suffix",
			secrets: []api.ResolvedSecret{
				{Name: "my-oauth", Type: "file", Target: "/home/gemini/.gemini/oauth_creds.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.Files == nil || auth.Files["OAuthCreds"] != "/home/gemini/.gemini/oauth_creds.json" {
					t.Errorf("Files[OAuthCreds] = %q, want oauth path", auth.Files["OAuthCreds"])
				}
			},
		},
		{
			name: "Codex by target suffix",
			secrets: []api.ResolvedSecret{
				{Name: "my-codex", Type: "file", Target: "/home/gemini/.codex/auth.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.Files == nil || auth.Files["CodexAuthFile"] != "/home/gemini/.codex/auth.json" {
					t.Errorf("Files[CodexAuthFile] = %q, want codex path", auth.Files["CodexAuthFile"])
				}
			},
		},
		{
			name: "OpenCode by target suffix",
			secrets: []api.ResolvedSecret{
				{Name: "my-opencode", Type: "file", Target: "/home/gemini/.local/share/opencode/auth.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.Files == nil || auth.Files["OpenCodeAuthFile"] != "/home/gemini/.local/share/opencode/auth.json" {
					t.Errorf("Files[OpenCodeAuthFile] = %q, want opencode path", auth.Files["OpenCodeAuthFile"])
				}
			},
		},
		{
			name: "Claude credentials by target suffix",
			secrets: []api.ResolvedSecret{
				{Name: "my-claude-creds", Type: "file", Target: "/home/agent/.claude/.credentials.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.Files == nil || auth.Files["ClaudeAuthFile"] == "" {
					t.Error("Files[ClaudeAuthFile] should be set from target suffix match")
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
			OverlayFileSecrets(&auth, tt.secrets, nil)
			tt.check(t, auth)
		})
	}
}

func TestOverlayFileSecrets_WithAuthMeta(t *testing.T) {
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
				if auth.Files == nil || auth.Files["ClaudeAuthFile"] != "/home/agent/.claude/.credentials.json" {
					t.Errorf("Files[ClaudeAuthFile] = %q, want credentials path", auth.Files["ClaudeAuthFile"])
				}
			},
		},
		{
			name: "config-driven field mapping for gcloud-adc (first-class field)",
			meta: claudeAuthMeta,
			secrets: []api.ResolvedSecret{
				{Name: "gcloud-adc", Type: "file", Target: "/home/agent/.config/gcloud/application_default_credentials.json"},
			},
			check: func(t *testing.T, auth api.AuthConfig) {
				if auth.GoogleAppCredentials != "/home/agent/.config/gcloud/application_default_credentials.json" {
					t.Errorf("GoogleAppCredentials = %q, want ADC path", auth.GoogleAppCredentials)
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
				if auth.Files == nil || auth.Files["ClaudeAuthFile"] != "/home/agent/.claude/.credentials.json" {
					t.Errorf("Files[ClaudeAuthFile] = %q, want credentials path from suffix fallback", auth.Files["ClaudeAuthFile"])
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
				if auth.Files != nil && auth.Files["ClaudeAuthFile"] != "" {
					t.Errorf("Files[ClaudeAuthFile] = %q, want empty (env-type should be skipped)", auth.Files["ClaudeAuthFile"])
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
				if auth.Files == nil || auth.Files["ClaudeAuthFile"] != "/home/agent/.claude/.credentials.json" {
					t.Errorf("Files[ClaudeAuthFile] = %q, want credentials path from suffix fallback", auth.Files["ClaudeAuthFile"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := api.AuthConfig{}
			OverlayFileSecrets(&auth, tt.secrets, tt.meta)
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
	t.Setenv("GOOGLE_CLOUD_REGION", "")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "")
	t.Setenv("CLOUD_ML_REGION", "")

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
