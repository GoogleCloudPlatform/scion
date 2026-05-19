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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	if s.DefaultTemplate != "default" {
		t.Errorf("expected default template 'default', got '%s'", s.DefaultTemplate)
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

func TestLoadSettingsKoanfWithProjectOverride(t *testing.T) {
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

func TestLoadSettingsKoanfV1ProjectID(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Unset SCION_HUB_ENDPOINT so it doesn't override the file-loaded value
	if orig, ok := os.LookupEnv("SCION_HUB_ENDPOINT"); ok {
		os.Unsetenv("SCION_HUB_ENDPOINT")
		t.Cleanup(func() { os.Setenv("SCION_HUB_ENDPOINT", orig) })
	}

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a v1 format settings file where grove_id is under hub.grove_id
	v1Settings := `schema_version: "1"
hub:
  enabled: true
  endpoint: "http://localhost:9810"
  grove_id: "test-grove-uuid-1234"
`
	if err := os.WriteFile(filepath.Join(groveScionDir, "settings.yaml"), []byte(v1Settings), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSettingsKoanf(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettingsKoanf failed: %v", err)
	}

	// The v1 hub.grove_id should be normalized to the top-level ProjectID
	if s.ProjectID != "test-grove-uuid-1234" {
		t.Errorf("expected top-level ProjectID 'test-grove-uuid-1234', got '%s'", s.ProjectID)
	}

	// Hub should still be populated
	if s.Hub == nil {
		t.Fatal("expected Hub config to be set")
	}
	if !*s.Hub.Enabled {
		t.Error("expected Hub to be enabled")
	}
	if s.Hub.Endpoint != "http://localhost:9810" {
		t.Errorf("expected Hub endpoint 'http://localhost:9810', got '%s'", s.Hub.Endpoint)
	}
}

func TestLoadSettingsKoanfV1ProjectIDHubWinsOverTopLevel(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a settings file with both top-level grove_id and hub.grove_id.
	// hub.grove_id is the canonical v1 location and should always take
	// precedence — this is critical for the merge scenario where global
	// sets top-level grove_id and the grove sets hub.grove_id.
	legacySettings := `grove_id: "top-level-id"
hub:
  enabled: true
  grove_id: "hub-level-id"
`
	if err := os.WriteFile(filepath.Join(groveScionDir, "settings.yaml"), []byte(legacySettings), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSettingsKoanf(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettingsKoanf failed: %v", err)
	}

	// hub.grove_id (canonical v1 location) should win
	if s.ProjectID != "hub-level-id" {
		t.Errorf("expected ProjectID 'hub-level-id' (from hub.grove_id), got '%s'", s.ProjectID)
	}
}

func TestLoadSettingsKoanfV1ProjectIDFromEnv(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Set SCION_HUB_GROVE_ID env var — should map to top-level grove_id
	os.Setenv("SCION_HUB_GROVE_ID", "env-grove-uuid")
	defer os.Unsetenv("SCION_HUB_GROVE_ID")

	s, err := LoadSettingsKoanf(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettingsKoanf failed: %v", err)
	}

	if s.ProjectID != "env-grove-uuid" {
		t.Errorf("expected ProjectID 'env-grove-uuid' from env var, got '%s'", s.ProjectID)
	}
}

func TestLoadSettingsKoanfV1BrokerFields(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a v1 format settings file where broker fields are under server.broker
	v1Settings := `schema_version: "1"
hub:
  enabled: true
  endpoint: "http://localhost:9810"
server:
  broker:
    broker_id: "test-broker-uuid"
    broker_token: "test-broker-token"
    broker_nickname: "my-test-broker"
`
	if err := os.WriteFile(filepath.Join(groveScionDir, "settings.yaml"), []byte(v1Settings), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSettingsKoanf(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettingsKoanf failed: %v", err)
	}

	// The v1 server.broker fields should be remapped to legacy hub fields
	if s.Hub == nil {
		t.Fatal("expected Hub config to be set")
	}
	if s.Hub.BrokerID != "test-broker-uuid" {
		t.Errorf("expected BrokerID 'test-broker-uuid', got '%s'", s.Hub.BrokerID)
	}
	if s.Hub.BrokerToken != "test-broker-token" {
		t.Errorf("expected BrokerToken 'test-broker-token', got '%s'", s.Hub.BrokerToken)
	}
	if s.Hub.BrokerNickname != "my-test-broker" {
		t.Errorf("expected BrokerNickname 'my-test-broker', got '%s'", s.Hub.BrokerNickname)
	}
}

func TestLoadSettingsKoanfV1BrokerFieldsNoOverrideExisting(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove")
	groveScionDir := filepath.Join(groveDir, ".scion")
	if err := os.MkdirAll(groveScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// When both legacy hub.brokerId and v1 server.broker.broker_id exist,
	// the legacy hub.brokerId should take precedence (not be overridden)
	settings := `hub:
  brokerId: "legacy-broker-id"
  brokerToken: "legacy-token"
server:
  broker:
    broker_id: "v1-broker-id"
    broker_token: "v1-token"
`
	if err := os.WriteFile(filepath.Join(groveScionDir, "settings.yaml"), []byte(settings), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSettingsKoanf(groveScionDir)
	if err != nil {
		t.Fatalf("LoadSettingsKoanf failed: %v", err)
	}

	// Legacy hub fields should take precedence
	if s.Hub.BrokerID != "legacy-broker-id" {
		t.Errorf("expected BrokerID 'legacy-broker-id', got '%s'", s.Hub.BrokerID)
	}
	if s.Hub.BrokerToken != "legacy-token" {
		t.Errorf("expected BrokerToken 'legacy-token', got '%s'", s.Hub.BrokerToken)
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

// TestV1ProjectIDSurvivesUpdateSetting verifies that grove_id written by
// writeGroveSettings in v1 format survives UpdateVersionedSetting round-trips.
// This is a regression test for the bug where grove_id was written at the
// top level (which VersionedSettings drops on unmarshal), then the first
// UpdateSetting call (e.g. hub.endpoint) would strip it, causing the global
// hub.grove_id to bleed into local groves.
func TestV1ProjectIDSurvivesUpdateSetting(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Unset env vars that could interfere
	for _, env := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_GROVE_ID"} {
		if orig, ok := os.LookupEnv(env); ok {
			os.Unsetenv(env)
			t.Cleanup(func() { os.Setenv(env, orig) })
		}
	}

	// Set up a global settings file with a different grove_id (simulating
	// a previously linked global grove).
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))
	globalSettings := `schema_version: "1"
hub:
  grove_id: "global-grove-id-should-not-bleed"
  endpoint: "https://hub.example.com"
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	// Simulate writeGroveSettings: create a v1 grove settings file with
	// grove_id under hub.grove_id (the correct v1 location).
	groveDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))
	groveSettings := `schema_version: "1"
active_profile: local
default_template: default
hub:
  grove_id: "local-grove-id-12345"
workspace_path: /tmp/my-project
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(groveSettings), 0644))

	// Verify the grove_id loads correctly before any updates.
	s, err := LoadSettingsKoanf(groveDir)
	require.NoError(t, err)
	assert.Equal(t, "local-grove-id-12345", s.ProjectID, "grove_id should come from local settings, not global")

	// Simulate what happens when the user runs "scion config set hub.endpoint"
	// or "scion hub enable" — this calls UpdateSetting which round-trips
	// through VersionedSettings.
	require.NoError(t, UpdateSetting(groveDir, "hub.endpoint", "https://hub.new.example.com", false))

	// Reload and verify grove_id survived the round-trip.
	s2, err := LoadSettingsKoanf(groveDir)
	require.NoError(t, err)
	assert.Equal(t, "local-grove-id-12345", s2.ProjectID, "grove_id must survive UpdateSetting round-trip")
	assert.Equal(t, "https://hub.new.example.com", s2.Hub.Endpoint, "hub endpoint should be updated")
}

func TestLoadSettingsKoanf_ProjectIDFileOverridesGlobal(t *testing.T) {
	// Simulates a git grove where grove_id is stored in a grove-id file
	// rather than in the settings file. The global settings have a different
	// hub.grove_id that should NOT bleed into the project grove.
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	for _, env := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_GROVE_ID"} {
		if orig, ok := os.LookupEnv(env); ok {
			os.Unsetenv(env)
			t.Cleanup(func() { os.Setenv(env, orig) })
		}
	}

	// Global settings with a grove_id (simulating a linked global grove)
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))
	globalSettings := `schema_version: "1"
hub:
  grove_id: "global-grove-id"
  enabled: true
  linked: true
  endpoint: "https://hub.example.com"
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	// Git grove .scion directory with grove-id file but NO grove_id in settings
	groveScionDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(groveScionDir, 0755))

	// Write the grove-id file (as initInRepoProject does)
	require.NoError(t, WriteProjectID(groveScionDir, "project-grove-id-from-file"))

	// Create a minimal grove settings file in the external config dir
	// (simulating ensureGroveSettingsFile which doesn't include grove_id)
	groveConfigDir, err := GetGitProjectExternalConfigDir(groveScionDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(groveConfigDir, 0755))
	groveSettings := `schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(groveConfigDir, "settings.yaml"), []byte(groveSettings), 0644))

	// Load settings for the project grove
	s, err := LoadSettingsKoanf(groveScionDir)
	require.NoError(t, err)

	// The grove-id file should take precedence over global hub.grove_id
	assert.Equal(t, "project-grove-id-from-file", s.ProjectID,
		"grove_id should come from grove-id file, not global settings")
}

func TestLoadSettingsKoanf_GlobalProjectIDDoesNotBleedIntoProject(t *testing.T) {
	// Verifies that when global settings have hub.grove_id set (from linking
	// the global grove) and a project grove also has its own hub.grove_id,
	// the project's value is used — not the global's.
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	for _, env := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_GROVE_ID"} {
		if orig, ok := os.LookupEnv(env); ok {
			os.Unsetenv(env)
			t.Cleanup(func() { os.Setenv(env, orig) })
		}
	}

	// Global settings with grove_id at top level (legacy format)
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))
	globalSettings := `grove_id: "global-grove-id-legacy"
hub:
  enabled: true
  endpoint: "https://hub.example.com"
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	// Project grove settings with hub.grove_id (v1 format)
	groveDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))
	groveSettings := `schema_version: "1"
hub:
  grove_id: "project-grove-id"
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(groveSettings), 0644))

	s, err := LoadSettingsKoanf(groveDir)
	require.NoError(t, err)

	// Project's hub.grove_id should override global's top-level grove_id
	assert.Equal(t, "project-grove-id", s.ProjectID,
		"grove_id should come from project hub.grove_id, not global top-level grove_id")
}

func TestLoadSettingsKoanf_V1HubProjectIDPopulatesGetHubProjectID(t *testing.T) {
	// Verifies that hub.grove_id (snake_case, V1 format) is remapped to
	// hub.groveId (camelCase) so that GetHubProjectID() returns the correct
	// value. Without this remapping, EnsureHubReady falls back to the local
	// grove_id and loops on grove registration when the IDs differ.
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	for _, env := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_GROVE_ID"} {
		if orig, ok := os.LookupEnv(env); ok {
			os.Unsetenv(env)
			t.Cleanup(func() { os.Setenv(env, orig) })
		}
	}

	// Create global dir to satisfy LoadSettingsKoanf
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	groveDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	// V1 format settings with hub.grove_id set (the hub grove ID)
	groveSettings := `schema_version: "1"
hub:
  enabled: true
  endpoint: "https://hub.example.com"
  grove_id: "hub-grove-uuid-972dd7f5"
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(groveSettings), 0644))

	s, err := LoadSettingsKoanf(groveDir)
	require.NoError(t, err)

	// GetHubProjectID() must return the hub grove ID from V1's hub.grove_id
	assert.Equal(t, "hub-grove-uuid-972dd7f5", s.GetHubProjectID(),
		"GetHubProjectID() should return the value from V1 hub.grove_id")
}

func TestLoadSettingsKoanf_V1HubProjectIDWithMarkerFile(t *testing.T) {
	// When a git grove has both a grove-id marker file (local deterministic ID)
	// and hub.grove_id in V1 settings (hub grove ID), the two must be distinct:
	// - settings.ProjectID should be the local ID (from the marker file)
	// - settings.GetHubProjectID() should be the hub ID (from hub.grove_id)
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	for _, env := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_GROVE_ID"} {
		if orig, ok := os.LookupEnv(env); ok {
			os.Unsetenv(env)
			t.Cleanup(func() { os.Setenv(env, orig) })
		}
	}

	// Create global dir
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	groveScionDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(groveScionDir, 0755))

	// Write grove-id marker file with local deterministic ID
	require.NoError(t, WriteProjectID(groveScionDir, "local-deterministic-id"))

	// For git groves, settings are stored in the external config dir.
	// Write V1 settings with hub.grove_id pointing to a different hub grove.
	groveConfigDir, err := GetGitProjectExternalConfigDir(groveScionDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(groveConfigDir, 0755))
	groveSettings := `schema_version: "1"
hub:
  enabled: true
  endpoint: "https://hub.example.com"
  grove_id: "hub-grove-uuid-different"
`
	require.NoError(t, os.WriteFile(filepath.Join(groveConfigDir, "settings.yaml"), []byte(groveSettings), 0644))

	s, err := LoadSettingsKoanf(groveScionDir)
	require.NoError(t, err)

	// ProjectID should come from the marker file (local deterministic ID)
	assert.Equal(t, "local-deterministic-id", s.ProjectID,
		"ProjectID should come from grove-id marker file")

	// GetHubProjectID() should return the hub grove ID from V1 settings
	assert.Equal(t, "hub-grove-uuid-different", s.GetHubProjectID(),
		"GetHubProjectID() should return the hub grove ID, distinct from local grove_id")
}
