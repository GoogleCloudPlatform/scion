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

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSettingsKoanf(t *testing.T) {
	// Create temporary directories for global and grove settings
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 1. Test defaults
	s, err := LoadSettingsKoanf(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettingsKoanf failed: %v", err)
	}
	if s.ActiveProfile != "local" {
		t.Errorf("expected active profile 'local', got '%s'", s.ActiveProfile)
	}
	if s.DefaultTemplate != "gemini" {
		t.Errorf("expected default template 'gemini', got '%s'", s.DefaultTemplate)
	}
}

func TestLoadSettingsKoanfWithYAML(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create global YAML settings
	globalScionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	globalSettingsYAML := `
active_profile: prod
default_template: claude
runtimes:
  kubernetes:
    namespace: scion-global
profiles:
  prod:
    runtime: kubernetes
    tmux: false
`
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSettingsKoanf(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettingsKoanf failed: %v", err)
	}
	if s.ActiveProfile != "prod" {
		t.Errorf("expected global override active_profile 'prod', got '%s'", s.ActiveProfile)
	}
	if s.DefaultTemplate != "claude" {
		t.Errorf("expected global override template 'claude', got '%s'", s.DefaultTemplate)
	}
	if s.Runtimes["kubernetes"].Namespace != "scion-global" {
		t.Errorf("expected global override runtime namespace 'scion-global', got '%s'", s.Runtimes["kubernetes"].Namespace)
	}
}

func TestLoadSettingsKoanfWithGroveOverride(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create global settings
	globalScionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	globalSettingsYAML := `
active_profile: prod
default_template: claude
`
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Create grove settings that override
	groveSettingsYAML := `
active_profile: local-dev
profiles:
  local-dev:
    runtime: docker
    tmux: true
`
	if err := os.WriteFile(filepath.Join(groveScionDir, "settings.yaml"), []byte(groveSettingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSettingsKoanf(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettingsKoanf failed: %v", err)
	}
	if s.ActiveProfile != "local-dev" {
		t.Errorf("expected grove override active_profile 'local-dev', got '%s'", s.ActiveProfile)
	}
	// Template should still be claude from global
	if s.DefaultTemplate != "claude" {
		t.Errorf("expected inherited global template 'claude', got '%s'", s.DefaultTemplate)
	}
}

func TestLoadSettingsKoanfWithEnvOverride(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Set environment variable override
	os.Setenv("SCION_ACTIVE_PROFILE", "remote")
	defer os.Unsetenv("SCION_ACTIVE_PROFILE")

	os.Setenv("SCION_DEFAULT_TEMPLATE", "opencode")
	defer os.Unsetenv("SCION_DEFAULT_TEMPLATE")

	s, err := LoadSettingsKoanf(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettingsKoanf failed: %v", err)
	}
	if s.ActiveProfile != "remote" {
		t.Errorf("expected env override active_profile 'remote', got '%s'", s.ActiveProfile)
	}
	if s.DefaultTemplate != "opencode" {
		t.Errorf("expected env override template 'opencode', got '%s'", s.DefaultTemplate)
	}
}

func TestLoadSettingsKoanfWithBucketEnvOverride(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Set bucket environment variable overrides
	os.Setenv("SCION_BUCKET_PROVIDER", "GCS")
	defer os.Unsetenv("SCION_BUCKET_PROVIDER")

	os.Setenv("SCION_BUCKET_NAME", "my-bucket")
	defer os.Unsetenv("SCION_BUCKET_NAME")

	os.Setenv("SCION_BUCKET_PREFIX", "agents")
	defer os.Unsetenv("SCION_BUCKET_PREFIX")

	s, err := LoadSettingsKoanf(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettingsKoanf failed: %v", err)
	}
	if s.Bucket == nil {
		t.Fatal("expected bucket config to be set from env vars")
	}
	if s.Bucket.Provider != "GCS" {
		t.Errorf("expected bucket provider 'GCS', got '%s'", s.Bucket.Provider)
	}
	if s.Bucket.Name != "my-bucket" {
		t.Errorf("expected bucket name 'my-bucket', got '%s'", s.Bucket.Name)
	}
	if s.Bucket.Prefix != "agents" {
		t.Errorf("expected bucket prefix 'agents', got '%s'", s.Bucket.Prefix)
	}
}

func TestGetSettingsPath(t *testing.T) {
	tmpDir := t.TempDir()

	// Test with no files
	if path := GetSettingsPath(tmpDir); path != "" {
		t.Errorf("expected empty path for no files, got '%s'", path)
	}

	// Test with YAML file
	yamlPath := filepath.Join(tmpDir, "settings.yaml")
	if err := os.WriteFile(yamlPath, []byte("active_profile: test"), 0644); err != nil {
		t.Fatal(err)
	}
	if path := GetSettingsPath(tmpDir); path != yamlPath {
		t.Errorf("expected '%s', got '%s'", yamlPath, path)
	}

	// Test with both YAML and JSON (YAML should be preferred)
	jsonPath := filepath.Join(tmpDir, "settings.json")
	if err := os.WriteFile(jsonPath, []byte(`{"active_profile": "json"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if path := GetSettingsPath(tmpDir); path != yamlPath {
		t.Errorf("expected YAML to be preferred '%s', got '%s'", yamlPath, path)
	}

	// Remove YAML, should fall back to JSON
	os.Remove(yamlPath)
	if path := GetSettingsPath(tmpDir); path != jsonPath {
		t.Errorf("expected JSON fallback '%s', got '%s'", jsonPath, path)
	}
}

func TestGetScionAgentConfigPath(t *testing.T) {
	tmpDir := t.TempDir()

	// Test with no files
	if path := GetScionAgentConfigPath(tmpDir); path != "" {
		t.Errorf("expected empty path for no files, got '%s'", path)
	}

	// Test with YAML file
	yamlPath := filepath.Join(tmpDir, "scion-agent.yaml")
	if err := os.WriteFile(yamlPath, []byte("harness: gemini"), 0644); err != nil {
		t.Fatal(err)
	}
	if path := GetScionAgentConfigPath(tmpDir); path != yamlPath {
		t.Errorf("expected '%s', got '%s'", yamlPath, path)
	}

	// Test with both YAML and JSON (YAML should be preferred)
	jsonPath := filepath.Join(tmpDir, "scion-agent.json")
	if err := os.WriteFile(jsonPath, []byte(`{"harness": "claude"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if path := GetScionAgentConfigPath(tmpDir); path != yamlPath {
		t.Errorf("expected YAML to be preferred '%s', got '%s'", yamlPath, path)
	}
}

func TestLoadSettingsKoanfWithJSONFallback(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create global JSON settings (backward compatibility)
	globalScionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	globalSettingsJSON := `{
		"active_profile": "json-profile",
		"default_template": "json-template"
	}`
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.json"), []byte(globalSettingsJSON), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSettingsKoanf(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettingsKoanf failed: %v", err)
	}
	if s.ActiveProfile != "json-profile" {
		t.Errorf("expected JSON fallback active_profile 'json-profile', got '%s'", s.ActiveProfile)
	}
	if s.DefaultTemplate != "json-template" {
		t.Errorf("expected JSON fallback template 'json-template', got '%s'", s.DefaultTemplate)
	}
}
