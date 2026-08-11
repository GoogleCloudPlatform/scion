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
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// --- Struct round-trip tests ---

func TestVersionedSettings_YAMLRoundTrip(t *testing.T) {
	autoHelp := true

	vs := &VersionedSettings{
		SchemaVersion:   "1",
		ActiveProfile:   "local",
		DefaultTemplate: "gemini",
		Hub: &V1HubClientConfig{
			Enabled:   boolPtr(true),
			Endpoint:  "https://hub.example.com",
			ProjectID: "test-grove",
		},
		CLI: &V1CLIConfig{
			AutoHelp:            &autoHelp,
			InteractiveDisabled: boolPtr(false),
		},
		Runtimes: map[string]V1RuntimeConfig{
			"docker":    {Type: "docker", Host: ""},
			"container": {Type: "container"},
		},
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {
				Harness: "gemini",
				Image:   "example.com/gemini:latest",
				User:    "scion",
				Model:   "gemini-2.5-pro",
				Args:    []string{"--sandbox=strict"},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {
				Runtime:              "container",
				DefaultTemplate:      "gemini",
				DefaultHarnessConfig: "gemini",
			},
		},
	}

	// Marshal to YAML
	data, err := yaml.Marshal(vs)
	require.NoError(t, err)

	// Validate against schema
	valErrors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, valErrors, "round-tripped YAML should validate against schema, got: %v", valErrors)

	// Unmarshal back
	var roundTripped VersionedSettings
	err = yaml.Unmarshal(data, &roundTripped)
	require.NoError(t, err)

	assert.Equal(t, vs.SchemaVersion, roundTripped.SchemaVersion)
	assert.Equal(t, vs.ActiveProfile, roundTripped.ActiveProfile)
	assert.Equal(t, vs.DefaultTemplate, roundTripped.DefaultTemplate)
	assert.Equal(t, vs.Hub.Endpoint, roundTripped.Hub.Endpoint)
	assert.Equal(t, vs.Hub.ProjectID, roundTripped.Hub.ProjectID)
	assert.Equal(t, vs.HarnessConfigs["gemini"].Model, roundTripped.HarnessConfigs["gemini"].Model)
	assert.Equal(t, vs.HarnessConfigs["gemini"].Args, roundTripped.HarnessConfigs["gemini"].Args)
	assert.Equal(t, vs.Profiles["local"].DefaultHarnessConfig, roundTripped.Profiles["local"].DefaultHarnessConfig)
}

// --- LoadVersionedSettings tests ---

func TestLoadVersionedSettings_DefaultsOnly(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "1", vs.SchemaVersion)
	assert.Equal(t, "local", vs.ActiveProfile)
	assert.Equal(t, "default", vs.DefaultTemplate)
	assert.Equal(t, "claude", vs.DefaultHarnessConfig)
	// harness_configs block is no longer in default settings (lives on disk as harness-config dirs)
	assert.Contains(t, vs.Runtimes, "docker")
	assert.Equal(t, "docker", vs.Runtimes["docker"].Type)
}

func TestLoadVersionedSettings_GlobalOverride(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	globalSettings := `
schema_version: "1"
active_profile: prod
default_template: claude
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "prod", vs.ActiveProfile)
	assert.Equal(t, "claude", vs.DefaultTemplate)
}

// TestLoadVersionedSettings_GlobalHarnessConfigNotOverriddenByProjectDefaults
// verifies that when a user sets default_harness_config and default_template in
// their global settings, initializing a project (which writes the embedded
// project defaults into the project settings file) does not override those
// global values.
//
// Regression test for GoogleCloudPlatform/scion#212.
func TestLoadVersionedSettings_GlobalHarnessConfigNotOverriddenByProjectDefaults(t *testing.T) {
	tmpDir := t.TempDir()

	t.Setenv("HOME", tmpDir)

	// Set up global settings with custom default_harness_config and default_template.
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	globalSettings := `
schema_version: "1"
default_harness_config: opencode
default_template: my-template
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	// Set up a project directory with the embedded project defaults — this is
	// exactly what scion init writes into the project settings file.
	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	projectDefaults, err := GetProjectDefaultSettingsYAML()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), projectDefaults, 0644))

	// Load merged settings. The project defaults must NOT override the user's
	// global preferences for default_harness_config and default_template.
	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "opencode", vs.DefaultHarnessConfig,
		"global default_harness_config should not be overridden by project defaults (issue #212)")
	assert.Equal(t, "my-template", vs.DefaultTemplate,
		"global default_template should not be overridden by project defaults")
}

func TestLoadVersionedSettings_GroveOverride(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	globalSettings := `
schema_version: "1"
active_profile: prod
default_template: claude
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	projectSettings := `
schema_version: "1"
active_profile: staging
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(projectSettings), 0644))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "staging", vs.ActiveProfile)
	// Template should still be claude from global
	assert.Equal(t, "claude", vs.DefaultTemplate)
}

func TestLoadVersionedSettings_EnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Set environment variable overrides
	_ = os.Setenv("SCION_ACTIVE_PROFILE", "remote")
	defer func() { _ = os.Unsetenv("SCION_ACTIVE_PROFILE") }()

	_ = os.Setenv("SCION_DEFAULT_TEMPLATE", "opencode")
	defer func() { _ = os.Unsetenv("SCION_DEFAULT_TEMPLATE") }()

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "remote", vs.ActiveProfile)
	assert.Equal(t, "opencode", vs.DefaultTemplate)
}

func TestLoadVersionedSettings_HubEnvVars(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Test SCION_HUB_GROVE_ID maps correctly (regression test)
	_ = os.Setenv("SCION_HUB_GROVE_ID", "my-grove-id")
	defer func() { _ = os.Unsetenv("SCION_HUB_GROVE_ID") }()

	_ = os.Setenv("SCION_HUB_LOCAL_ONLY", "true")
	defer func() { _ = os.Unsetenv("SCION_HUB_LOCAL_ONLY") }()

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	require.NotNil(t, vs.Hub)
	assert.Equal(t, "my-grove-id", vs.Hub.ProjectID)
}

func TestLoadVersionedSettings_CLIEnvVars(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	_ = os.Setenv("SCION_CLI_AUTOHELP", "false")
	defer func() { _ = os.Unsetenv("SCION_CLI_AUTOHELP") }()

	_ = os.Setenv("SCION_CLI_INTERACTIVE_DISABLED", "true")
	defer func() { _ = os.Unsetenv("SCION_CLI_INTERACTIVE_DISABLED") }()

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	require.NotNil(t, vs.CLI)
}

func TestLoadVersionedSettings_JSONFallback(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	// Write JSON settings (should load via JSON fallback)
	globalJSON := `{
		"schema_version": "1",
		"active_profile": "json-profile",
		"default_template": "json-template"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.json"), []byte(globalJSON), 0644))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "json-profile", vs.ActiveProfile)
	assert.Equal(t, "json-template", vs.DefaultTemplate)
}

func TestLoadVersionedSettings_NewFields(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	projectSettings := `
schema_version: "1"
harness_configs:
  gemini-custom:
    harness: gemini
    image: example.com/gemini:v2
    user: scion
    model: gemini-2.5-pro
    args: ["--sandbox=strict", "--verbose"]
runtimes:
  my-docker:
    type: docker
    host: tcp://remote:2376
profiles:
  custom:
    runtime: my-docker
    default_template: gemini
    default_harness_config: gemini-custom
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(projectSettings), 0644))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	// Check new harness config fields
	hc, ok := vs.HarnessConfigs["gemini-custom"]
	require.True(t, ok)
	assert.Equal(t, "gemini", hc.Harness)
	assert.Equal(t, "gemini-2.5-pro", hc.Model)
	assert.Equal(t, []string{"--sandbox=strict", "--verbose"}, hc.Args)

	// Check runtime type field
	rt, ok := vs.Runtimes["my-docker"]
	require.True(t, ok)
	assert.Equal(t, "docker", rt.Type)
	assert.Equal(t, "tcp://remote:2376", rt.Host)

	// Check new profile fields
	profile, ok := vs.Profiles["custom"]
	require.True(t, ok)
	assert.Equal(t, "gemini", profile.DefaultTemplate)
	assert.Equal(t, "gemini-custom", profile.DefaultHarnessConfig)
}

// --- AdaptLegacySettings tests ---

func TestAdaptLegacySettings_FullMapping(t *testing.T) {
	autoHelp := true
	enabled := true

	legacy := &Settings{
		ActiveProfile:   "local",
		DefaultTemplate: "gemini",
		Hub: &HubClientConfig{
			Enabled:   &enabled,
			Endpoint:  "https://hub.example.com",
			ProjectID: "test-grove",
		},
		CLI: &CLIConfig{
			AutoHelp: &autoHelp,
		},
		Runtimes: map[string]RuntimeConfig{
			"docker":    {Host: "tcp://localhost:2375"},
			"container": {},
		},
		Harnesses: map[string]HarnessConfig{
			"gemini": {Image: "example.com/gemini:latest", User: "scion"},
			"claude": {Image: "example.com/claude:latest", User: "scion"},
		},
		Profiles: map[string]ProfileConfig{
			"local": {Runtime: "container"},
		},
	}

	vs, warnings := AdaptLegacySettings(legacy)

	assert.Equal(t, "1", vs.SchemaVersion)
	assert.Equal(t, "local", vs.ActiveProfile)
	assert.Equal(t, "gemini", vs.DefaultTemplate)

	// Hub mapping
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "https://hub.example.com", vs.Hub.Endpoint)
	assert.Equal(t, "test-grove", vs.Hub.ProjectID)
	assert.True(t, *vs.Hub.Enabled)

	// CLI mapping
	require.NotNil(t, vs.CLI)
	assert.True(t, *vs.CLI.AutoHelp)
	assert.Nil(t, vs.CLI.InteractiveDisabled) // New field, should be nil

	// Runtime type inference
	assert.Equal(t, "docker", vs.Runtimes["docker"].Type)
	assert.Equal(t, "container", vs.Runtimes["container"].Type)
	assert.Equal(t, "tcp://localhost:2375", vs.Runtimes["docker"].Host)

	// Harness → HarnessConfig mapping
	assert.Equal(t, "gemini", vs.HarnessConfigs["gemini"].Harness)
	assert.Equal(t, "example.com/gemini:latest", vs.HarnessConfigs["gemini"].Image)
	assert.Equal(t, "claude", vs.HarnessConfigs["claude"].Harness)

	// Profile mapping — new fields should be zero
	assert.Equal(t, "container", vs.Profiles["local"].Runtime)
	assert.Equal(t, "", vs.Profiles["local"].DefaultTemplate)
	assert.Equal(t, "", vs.Profiles["local"].DefaultHarnessConfig)

	// Should have warning about harnesses rename
	assert.NotEmpty(t, warnings)
	hasHarnessWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "harnesses is deprecated") {
			hasHarnessWarning = true
			break
		}
	}
	assert.True(t, hasHarnessWarning, "should warn about harnesses deprecation")
}

func TestAdaptLegacySettings_HubFieldWarnings(t *testing.T) {
	legacy := &Settings{
		Hub: &HubClientConfig{
			Token:          "secret-token",
			APIKey:         "api-key",
			BrokerID:       "broker-123",
			BrokerNickname: "my-broker",
			BrokerToken:    "broker-token",
			LastSyncedAt:   "2024-01-01T00:00:00Z",
		},
	}

	vs, warnings := AdaptLegacySettings(legacy)

	// These fields should NOT be in the versioned settings
	assert.NotNil(t, vs.Hub)

	// Should have warnings for all deprecated fields
	warningTexts := map[string]bool{
		"hub.token":          false,
		"hub.apiKey":         false,
		"hub.brokerId":       false,
		"hub.brokerNickname": false,
		"hub.brokerToken":    false,
		"hub.lastSyncedAt":   false,
	}
	for _, w := range warnings {
		for key := range warningTexts {
			if strings.Contains(w, key) {
				warningTexts[key] = true
			}
		}
	}
	for key, found := range warningTexts {
		assert.True(t, found, "expected warning about %s", key)
	}
}

func TestAdaptLegacySettings_BucketWarning(t *testing.T) {
	legacy := &Settings{
		Bucket: &BucketConfig{
			Provider: "GCS",
			Name:     "my-bucket",
			Prefix:   "agents",
		},
	}

	_, warnings := AdaptLegacySettings(legacy)

	hasBucketWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "bucket") {
			hasBucketWarning = true
			break
		}
	}
	assert.True(t, hasBucketWarning, "should warn about bucket config deprecation")
}

func TestAdaptLegacySettings_NilInput(t *testing.T) {
	vs, warnings := AdaptLegacySettings(nil)

	assert.Equal(t, "1", vs.SchemaVersion)
	assert.Empty(t, warnings)
}

func TestAdaptLegacySettings_EmptyFields(t *testing.T) {
	legacy := &Settings{}

	vs, warnings := AdaptLegacySettings(legacy)

	assert.Equal(t, "1", vs.SchemaVersion)
	assert.Nil(t, vs.Hub)
	assert.Nil(t, vs.CLI)
	assert.Nil(t, vs.Runtimes)
	assert.Nil(t, vs.HarnessConfigs)
	assert.Nil(t, vs.Profiles)
	assert.Empty(t, warnings)
}

// --- convertVersionedToLegacy tests ---

func TestConvertVersionedToLegacy(t *testing.T) {
	vs := &VersionedSettings{
		SchemaVersion:   "1",
		ActiveProfile:   "local",
		DefaultTemplate: "gemini",
		Hub: &V1HubClientConfig{
			Enabled:   boolPtr(true),
			Endpoint:  "https://hub.example.com",
			ProjectID: "test-grove",
		},
		CLI: &V1CLIConfig{
			AutoHelp:            boolPtr(true),
			InteractiveDisabled: boolPtr(false),
		},
		Server: &V1ServerConfig{
			Broker: &V1BrokerConfig{
				BrokerID:       "broker-456",
				BrokerToken:    "broker-token-xyz",
				BrokerNickname: "my-broker",
			},
		},
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {Type: "docker", Host: "tcp://localhost:2375"},
		},
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {
				Harness: "gemini",
				Image:   "example.com/gemini:latest",
				User:    "scion",
				Model:   "gemini-2.5-pro",
				Args:    []string{"--sandbox"},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {
				Runtime:              "docker",
				DefaultTemplate:      "gemini",
				DefaultHarnessConfig: "gemini",
			},
		},
	}

	legacy := convertVersionedToLegacy(vs)

	assert.Equal(t, "local", legacy.ActiveProfile)
	assert.Equal(t, "gemini", legacy.DefaultTemplate)

	// Hub — only v1 fields should be mapped
	require.NotNil(t, legacy.Hub)
	assert.Equal(t, "https://hub.example.com", legacy.Hub.Endpoint)
	assert.Equal(t, "test-grove", legacy.Hub.ProjectID)
	assert.True(t, *legacy.Hub.Enabled)
	assert.Empty(t, legacy.Hub.Token) // Not in v1

	// Broker fields from Server.Broker should be mapped to Hub
	assert.Equal(t, "broker-456", legacy.Hub.BrokerID)
	assert.Equal(t, "broker-token-xyz", legacy.Hub.BrokerToken)
	assert.Equal(t, "my-broker", legacy.Hub.BrokerNickname)

	// CLI — InteractiveDisabled should not be in legacy
	require.NotNil(t, legacy.CLI)
	assert.True(t, *legacy.CLI.AutoHelp)

	// Runtimes — Type should be dropped
	assert.Equal(t, "tcp://localhost:2375", legacy.Runtimes["docker"].Host)

	// Harnesses — Model and Args should be dropped
	assert.Equal(t, "example.com/gemini:latest", legacy.Harnesses["gemini"].Image)

	// Profiles — new fields should be dropped
	assert.Equal(t, "docker", legacy.Profiles["local"].Runtime)
}

func TestConvertVersionedToLegacy_BrokerWithoutHub(t *testing.T) {
	// When Hub is nil but Server.Broker has fields, Hub should be created
	vs := &VersionedSettings{
		SchemaVersion: "1",
		Server: &V1ServerConfig{
			Broker: &V1BrokerConfig{
				BrokerID:    "broker-789",
				BrokerToken: "token-abc",
			},
		},
	}

	legacy := convertVersionedToLegacy(vs)

	require.NotNil(t, legacy.Hub, "Hub should be created when Server.Broker has fields")
	assert.Equal(t, "broker-789", legacy.Hub.BrokerID)
	assert.Equal(t, "token-abc", legacy.Hub.BrokerToken)
}

func TestConvertVersionedToLegacy_Nil(t *testing.T) {
	legacy := convertVersionedToLegacy(nil)
	assert.NotNil(t, legacy)
	assert.Empty(t, legacy.ActiveProfile)
}

// --- LoadEffectiveSettings tests ---

func TestLoadEffectiveSettings_VersionedFileRouting(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Write versioned project settings
	projectSettings := `
schema_version: "1"
active_profile: versioned-profile
harness_configs:
  gemini:
    harness: gemini
    image: example.com/gemini:latest
    user: scion
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(projectSettings), 0644))

	vs, warnings, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "versioned-profile", vs.ActiveProfile)
	assert.Empty(t, warnings, "versioned path should produce no deprecation warnings")
}

func TestLoadEffectiveSettings_LegacyFileRouting(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Write legacy project settings (has harnesses, no schema_version)
	projectSettings := `
active_profile: legacy-profile
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
profiles:
  legacy-profile:
    runtime: docker
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(projectSettings), 0644))

	vs, warnings, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "legacy-profile", vs.ActiveProfile)
	assert.Equal(t, "1", vs.SchemaVersion) // Should be set by adapter
	assert.NotEmpty(t, warnings, "legacy path should produce deprecation warnings")
}

func TestLoadEffectiveSettings_NoUserFiles(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// No settings files — should use defaults via legacy path
	vs, warnings, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, "local", vs.ActiveProfile)
	assert.Equal(t, "default", vs.DefaultTemplate)
	// Defaults flow through legacy path since no user files, so we get harness warnings
	// from the adaptation of embedded defaults
	_ = warnings
}

func TestLoadEffectiveSettings_WarnOnIgnoredSettingsFile(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	t.Run("non-empty file without schema_version produces warning", func(t *testing.T) {
		// Write settings with real keys but no schema_version, no harnesses,
		// and no v1 runtime indicators — this file will be silently ignored.
		settingsContent := "default_template: my-template\n"
		settingsPath := filepath.Join(projectDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte(settingsContent), 0644))
		defer func() { _ = os.Remove(settingsPath) }()

		_, warnings, err := LoadEffectiveSettings(projectDir)
		require.NoError(t, err)

		found := false
		for _, w := range warnings {
			if strings.Contains(w, "no schema_version field") && strings.Contains(w, settingsPath) {
				found = true
				break
			}
		}
		assert.True(t, found, "expected warning about missing schema_version, got warnings: %v", warnings)
	})

	t.Run("empty file does not produce warning", func(t *testing.T) {
		settingsPath := filepath.Join(projectDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte(""), 0644))
		defer func() { _ = os.Remove(settingsPath) }()

		_, warnings, err := LoadEffectiveSettings(projectDir)
		require.NoError(t, err)

		for _, w := range warnings {
			assert.NotContains(t, w, "no schema_version field",
				"empty file should not produce schema_version warning")
		}
	})

	t.Run("file with only comments does not produce warning", func(t *testing.T) {
		settingsPath := filepath.Join(projectDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte("# just a comment\n"), 0644))
		defer func() { _ = os.Remove(settingsPath) }()

		_, warnings, err := LoadEffectiveSettings(projectDir)
		require.NoError(t, err)

		for _, w := range warnings {
			assert.NotContains(t, w, "no schema_version field",
				"comment-only file should not produce schema_version warning")
		}
	})

	t.Run("legacy format file does not produce warning", func(t *testing.T) {
		// Write a file with harnesses key (legacy format)
		settingsPath := filepath.Join(projectDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte("harnesses:\n  claude:\n    image: test\n"), 0644))
		defer func() { _ = os.Remove(settingsPath) }()

		_, warnings, err := LoadEffectiveSettings(projectDir)
		require.NoError(t, err)
		for _, w := range warnings {
			assert.NotContains(t, w, "no schema_version field",
				"legacy format file should not produce schema_version warning")
		}
	})
}

// --- Default settings compatibility tests ---

func TestGetDefaultSettingsData_ProducesSameEffectiveDefaults(t *testing.T) {
	// GetDefaultSettingsData should produce the same effective config regardless
	// of whether the embedded file is versioned or legacy
	data, err := GetDefaultSettingsData()
	require.NoError(t, err)

	var settings Settings
	require.NoError(t, json.Unmarshal(data, &settings))

	// Harness configs are no longer inline in settings (they live on disk as harness-config dirs)
	// So the Harnesses map should be empty or nil
	assert.Empty(t, settings.Harnesses, "harness configs should not be inline in default settings")

	// Should have expected runtimes
	assert.Contains(t, settings.Runtimes, "docker")
	assert.Contains(t, settings.Runtimes, "container")
	assert.Contains(t, settings.Runtimes, "kubernetes")

	// Should have expected profiles
	assert.Contains(t, settings.Profiles, "local")
	assert.Contains(t, settings.Profiles, "remote")

	// OS-specific runtime check
	expectedRuntime := "docker"
	if runtime.GOOS == "darwin" {
		expectedRuntime = "container"
	}
	assert.Equal(t, expectedRuntime, settings.Profiles["local"].Runtime)
}

func TestDefaultSettingsValidateAgainstSchema(t *testing.T) {
	// The embedded default_settings.yaml should validate against the v1 schema
	data, err := EmbedsFS.ReadFile("embeds/default_settings.yaml")
	require.NoError(t, err)

	valErrors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, valErrors, "default settings should validate against v1 schema, got: %v", valErrors)
}

func TestDefaultSettingsDataYAML_OSAdjustment(t *testing.T) {
	data, err := GetDefaultSettingsDataYAML()
	require.NoError(t, err)

	// Parse as versioned settings to check OS adjustment
	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))

	expectedRuntime := "docker"
	if runtime.GOOS == "darwin" {
		expectedRuntime = "container"
	}

	localProfile, ok := vs.Profiles["local"]
	require.True(t, ok, "local profile should exist")
	assert.Equal(t, expectedRuntime, localProfile.Runtime)
}

// --- Adapter round-trip consistency ---

func TestAdapterRoundTripConsistency(t *testing.T) {
	// Load defaults via legacy path + adapt, vs load directly via versioned
	// The results should be equivalent in the shared fields
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Load via legacy path
	legacySettings, err := LoadSettingsKoanf(projectDir)
	require.NoError(t, err)
	adapted, _ := AdaptLegacySettings(legacySettings)

	// Load via versioned path
	versioned, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	// Compare shared fields
	assert.Equal(t, adapted.ActiveProfile, versioned.ActiveProfile)
	assert.Equal(t, adapted.DefaultTemplate, versioned.DefaultTemplate)

	// Compare harness config images (adapted from legacy harnesses)
	for name, hc := range adapted.HarnessConfigs {
		vhc, ok := versioned.HarnessConfigs[name]
		if assert.True(t, ok, "versioned should have harness config %q", name) {
			assert.Equal(t, hc.Image, vhc.Image, "image mismatch for %q", name)
			assert.Equal(t, hc.User, vhc.User, "user mismatch for %q", name)
		}
	}

	// Compare profiles
	for name, profile := range adapted.Profiles {
		vProfile, ok := versioned.Profiles[name]
		if assert.True(t, ok, "versioned should have profile %q", name) {
			assert.Equal(t, profile.Runtime, vProfile.Runtime, "runtime mismatch for profile %q", name)
		}
	}
}

// --- resolveEffectiveProjectPath tests ---

func TestResolveEffectiveProjectPath_Global(t *testing.T) {
	result := resolveEffectiveProjectPath("global")
	assert.Equal(t, "", result, "global should resolve to empty (already loaded)")

	result = resolveEffectiveProjectPath("home")
	assert.Equal(t, "", result, "home should resolve to empty (already loaded)")
}

func TestResolveEffectiveProjectPath_Explicit(t *testing.T) {
	// A plain .scion path with no grove-id → returned as-is (non-git grove)
	result := resolveEffectiveProjectPath("/some/path/.scion")
	assert.Equal(t, "/some/path/.scion", result)
}

func TestResolveEffectiveProjectPath_GitProject(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Simulate a git grove with grove-id → should redirect to external config dir
	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	_ = os.MkdirAll(projectDir, 0755)
	_ = WriteProjectID(projectDir, "550e8400-e29b-41d4-a716-446655440000")

	result := resolveEffectiveProjectPath(projectDir)

	want := filepath.Join(tmpHome, ".scion", "project-configs", "my-repo__550e8400", ".scion")
	assert.Equal(t, want, result)
}

// --- versionedEnvKeyMapper tests ---

func TestVersionedEnvKeyMapper(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"SCION_ACTIVE_PROFILE", "active_profile"},
		{"SCION_DEFAULT_TEMPLATE", "default_template"},
		{"SCION_HUB_ENDPOINT", "hub.endpoint"},
		{"SCION_HUB_GROVE_ID", "hub.grove_id"},
		{"SCION_HUB_LOCAL_ONLY", "hub.local_only"},
		{"SCION_HUB_ENABLED", "hub.enabled"},
		{"SCION_CLI_AUTOHELP", "cli.autohelp"},
		{"SCION_CLI_INTERACTIVE_DISABLED", "cli.interactive_disabled"},
		{"SCION_SERVER_ENV", "server.env"},
		{"SCION_SERVER_LOG_LEVEL", "server.log_level"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := versionedEnvKeyMapper(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// --- detectHierarchyFormat tests ---

func TestDetectHierarchyFormat_Versioned(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()
	_ = os.Chdir(tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	versionedSettings := `schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(versionedSettings), 0644))

	hasVersioned, missingSchemaVersion := detectHierarchyFormat("")
	assert.True(t, hasVersioned)
	assert.False(t, missingSchemaVersion)
}

func TestDetectHierarchyFormat_Legacy(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()
	_ = os.Chdir(tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	legacySettings := `active_profile: local
harnesses:
  gemini:
    image: test
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(legacySettings), 0644))

	hasVersioned, missingSchemaVersion := detectHierarchyFormat("")
	assert.False(t, hasVersioned)
	assert.False(t, missingSchemaVersion)
}

func TestDetectHierarchyFormat_NoFiles(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()
	_ = os.Chdir(tmpDir)

	hasVersioned, missingSchemaVersion := detectHierarchyFormat("")
	assert.False(t, hasVersioned)
	assert.False(t, missingSchemaVersion)
}

func TestDetectHierarchyFormat_GroveVersioned(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()
	_ = os.Chdir(tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Global is legacy, project is versioned
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	legacySettings := `active_profile: local
harnesses:
  gemini:
    image: test
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(legacySettings), 0644))

	versionedSettings := `schema_version: "1"
active_profile: custom
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(versionedSettings), 0644))

	hasVersioned, missingSchemaVersion := detectHierarchyFormat(projectDir)
	assert.True(t, hasVersioned)
	assert.False(t, missingSchemaVersion)
}

func TestDetectHierarchyFormat_V1StructuralIndicators(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()
	_ = os.Chdir(tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	// v1-shaped but missing schema_version
	v1ShapedSettings := `active_profile: cr
runtimes:
  cr:
    type: cloudrun
    cloudrun:
      project: my-project
      region: us-central1
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(v1ShapedSettings), 0644))

	hasVersioned, missingSchemaVersion := detectHierarchyFormat("")
	assert.True(t, hasVersioned, "v1-shaped file without schema_version should be detected as versioned")
	assert.True(t, missingSchemaVersion, "should flag that schema_version is missing")
}

// --- ResolveHarnessConfig tests ---

func TestResolveHarnessConfig_Default(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "local",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {
				Harness: "gemini",
				Image:   "example.com/gemini:latest",
				User:    "scion",
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}

	hc, err := vs.ResolveHarnessConfig("", "gemini")
	require.NoError(t, err)
	assert.Equal(t, "example.com/gemini:latest", hc.Image)
	assert.Equal(t, "scion", hc.User)
	assert.Equal(t, "gemini", hc.Harness)
}

func TestResolveHarnessConfig_Named(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "local",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {
				Harness: "gemini",
				Image:   "example.com/gemini:latest",
				User:    "scion",
			},
			"gemini-high-security": {
				Harness: "gemini",
				Image:   "example.com/gemini:hardened",
				User:    "restricted",
				Model:   "gemini-2.5-pro",
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}

	hc, err := vs.ResolveHarnessConfig("local", "gemini-high-security")
	require.NoError(t, err)
	assert.Equal(t, "example.com/gemini:hardened", hc.Image)
	assert.Equal(t, "restricted", hc.User)
	assert.Equal(t, "gemini", hc.Harness)
	assert.Equal(t, "gemini-2.5-pro", hc.Model)
}

func TestResolveHarnessConfig_WithProfileOverrides(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "staging",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {
				Harness: "gemini",
				Image:   "example.com/gemini:latest",
				User:    "scion",
				Env:     map[string]string{"BASE_KEY": "base_value"},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"staging": {
				Runtime: "docker",
				Env:     map[string]string{"PROFILE_KEY": "profile_value"},
				Volumes: []api.VolumeMount{{Source: "/profile/vol", Target: "/mnt/vol"}},
				HarnessOverrides: map[string]V1HarnessOverride{
					"gemini": {
						Image: "example.com/gemini:staging",
						Env:   map[string]string{"OVERRIDE_KEY": "override_value"},
					},
				},
			},
		},
	}

	hc, err := vs.ResolveHarnessConfig("", "gemini")
	require.NoError(t, err)
	assert.Equal(t, "example.com/gemini:staging", hc.Image, "image should be overridden by profile")
	assert.Equal(t, "scion", hc.User, "user should remain from base config")
	assert.Equal(t, "base_value", hc.Env["BASE_KEY"], "base env should be preserved")
	// G3-full: profiles.<p>.env is no longer merged. The fixture still SETS
	// PROFILE_KEY above — leave it there, it is the injected value whose absence
	// this asserts. Before G3-full this line read
	//   assert.Equal(t, "profile_value", hc.Env["PROFILE_KEY"], "profile env should be merged")
	// and the removal is a breaking change, not a correction of a wrong expectation.
	assert.NotContains(t, hc.Env, "PROFILE_KEY", "G3-full: profile env is no longer an injection point")
	// harness_overrides env SURVIVES G3-full — unchanged, and deliberately so.
	assert.Equal(t, "override_value", hc.Env["OVERRIDE_KEY"], "override env should be merged")
	assert.Len(t, hc.Volumes, 1, "profile volume should be appended")
	assert.Equal(t, "/mnt/vol", hc.Volumes[0].Target)
}

// TestResolveHarnessConfig_ProfileEnvNotMerged pins the G3-full removal: the
// profiles.<p>.env injection point no longer exists in ResolveHarnessConfig.
//
// Read the fixture carefully before changing it; three separate traps apply.
//
//  1. EXISTENCE CONTROL. An "env key is absent" assertion passes for free if the
//     profile was never located at all — ResolveHarnessConfig returns baseConfig
//     early when vs.Profiles[profileName] is missing, and an empty profileName
//     falls back to ActiveProfile. So this test asserts the profile's VOLUME is
//     appended. That merge sits in the same block the deleted env merge sat in,
//     and it proves the profile was found and applied. Without it the absence
//     below measures nothing.
//
//  2. THE HARNESS OVERRIDE IS DELIBERATELY POPULATED, AND ONLY FOR ONE KEY.
//     profiles.<p>.harness_overrides.<hc>.env is merged AFTER profile env and
//     therefore OUTRANKS it, so it can mask this deletion — but Env is a
//     per-key map and mergeMaps overlays key by key, so it masks ONLY the keys
//     it itself sets. PROFILE_ONLY_KEY is set by the profile and NOT by the
//     override, which is what keeps the deletion observable.
//
//     Leaving the override unset entirely would also work, but this fixture is
//     strictly better: it additionally pins that harness_overrides SURVIVES
//     G3-full, and it exercises the both-populated state, which is the real
//     configuration users are left in after the migration.
//
//  3. SHARED_KEY is NOT asserting "harness-config env replaces profile env".
//     That claim was in the findings matrix, it was backwards, and it was
//     retracted: before G3-full the profile value WON this key. The assertion
//     below says the base value is left undisturbed because the merge that used
//     to overwrite it is gone. Nothing remains to be replaced, so the removal
//     does not vindicate the old claim — it makes it vacuous.
func TestResolveHarnessConfig_ProfileEnvNotMerged(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "dev",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {
				Harness: "gemini",
				Image:   "example.com/gemini:latest",
				Env: map[string]string{
					"SHARED_KEY": "from-harness-config",
				},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"dev": {
				Runtime: "docker",
				Env: map[string]string{
					"SHARED_KEY":       "from-profile",
					"PROFILE_ONLY_KEY": "from-profile",
				},
				// Populated, but NOT for PROFILE_ONLY_KEY — see trap 2 above.
				HarnessOverrides: map[string]V1HarnessOverride{
					"gemini": {Env: map[string]string{"OVERLAP_KEY": "from-override"}},
				},
				Volumes: []api.VolumeMount{{Source: "/profile/vol", Target: "/mnt/profile"}},
			},
		},
	}

	// profileName passed explicitly rather than relying on ActiveProfile.
	hc, err := vs.ResolveHarnessConfig("dev", "gemini")
	require.NoError(t, err)

	// Existence control — must come first. If this fails, every env assertion
	// below is vacuous and the test result means nothing.
	require.Len(t, hc.Volumes, 1, "existence control: the profile must have been found and merged")
	require.Equal(t, "/mnt/profile", hc.Volumes[0].Target, "existence control: the merged volume must be the profile's")

	// The removal itself: a key that ONLY profile env supplied is gone.
	assert.NotContains(t, hc.Env, "PROFILE_ONLY_KEY",
		"G3-full: profiles.<p>.env is no longer an injection point, so a profile-only key must not appear")

	// The base value survives untouched. See trap 3 — this is not a claim that
	// harness-config env outranks profile env.
	assert.Equal(t, "from-harness-config", hc.Env["SHARED_KEY"],
		"harness_configs.<hc>.env must be left undisturbed now that the profile merge is gone")

	// The two surviving env sources, pinned here so this test distinguishes
	// "G3-full removed a rank" from "G3-full removed the tier". Without these a
	// change that wiped out all settings env would still pass the assertion above.
	assert.Equal(t, "from-override", hc.Env["OVERLAP_KEY"],
		"profiles.<p>.harness_overrides.<hc>.env SURVIVES G3-full")
	assert.Contains(t, hc.Env, "SHARED_KEY",
		"harness_configs.<hc>.env SURVIVES G3-full — it is the migration path for the removed profile env")
}

func TestResolveHarnessConfig_NotFound(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "local",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {Harness: "gemini", Image: "test"},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}

	// When the config name is not found, an empty base should be returned (not an error)
	hc, err := vs.ResolveHarnessConfig("", "nonexistent")
	require.NoError(t, err)
	assert.Empty(t, hc.Image, "empty base should have no image")
	assert.Empty(t, hc.User, "empty base should have no user")
	assert.Empty(t, hc.Harness, "empty base should have no harness")
}

func TestResolveHarnessConfig_NotFoundWithProfileOverrides(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "staging",
		Profiles: map[string]V1ProfileConfig{
			"staging": {
				HarnessOverrides: map[string]V1HarnessOverride{
					"custom": {
						Image: "custom-image:latest",
						Env:   map[string]string{"KEY": "val"},
					},
				},
			},
		},
	}

	// Even when base config not found, profile overrides should apply
	hc, err := vs.ResolveHarnessConfig("", "custom")
	require.NoError(t, err)
	assert.Equal(t, "custom-image:latest", hc.Image)
	assert.Equal(t, "val", hc.Env["KEY"])
}

func TestResolveHarnessConfig_ProfileNotFound(t *testing.T) {
	// When the profile is not found, we should still return the base config without error.
	vs := &VersionedSettings{
		ActiveProfile: "missing-profile",
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {Harness: "gemini", Image: "test", User: "scion"},
		},
		Profiles: map[string]V1ProfileConfig{},
	}

	hc, err := vs.ResolveHarnessConfig("", "gemini")
	require.NoError(t, err)
	assert.Equal(t, "test", hc.Image)
	assert.Equal(t, "scion", hc.User)
}

// --- ResolveRuntime tests ---

func TestResolveRuntime_Basic(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "local",
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {Type: "docker", Host: "tcp://localhost:2375"},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}

	rtConfig, runtimeType, err := vs.ResolveRuntime("")
	require.NoError(t, err)
	assert.Equal(t, "docker", runtimeType)
	assert.Equal(t, "tcp://localhost:2375", rtConfig.Host)
}

func TestResolveRuntime_WithType(t *testing.T) {
	// Runtime with explicit Type field different from map key
	vs := &VersionedSettings{
		ActiveProfile: "remote",
		Runtimes: map[string]V1RuntimeConfig{
			"my-remote-cluster": {
				Type:      "kubernetes",
				Namespace: "scion",
				Context:   "prod-cluster",
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"remote": {Runtime: "my-remote-cluster"},
		},
	}

	rtConfig, runtimeType, err := vs.ResolveRuntime("")
	require.NoError(t, err)
	assert.Equal(t, "kubernetes", runtimeType, "should use explicit Type field")
	assert.Equal(t, "scion", rtConfig.Namespace)
	assert.Equal(t, "prod-cluster", rtConfig.Context)
}

func TestResolveRuntime_TypeFromKey(t *testing.T) {
	// Type field absent — should fall back to map key name
	vs := &VersionedSettings{
		ActiveProfile: "local",
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {Host: "unix:///var/run/docker.sock"},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}

	_, runtimeType, err := vs.ResolveRuntime("")
	require.NoError(t, err)
	assert.Equal(t, "docker", runtimeType, "should fall back to map key name when Type is empty")
}

func TestResolveRuntime_ProfileEnvMerge(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "local",
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {
				Type: "docker",
				Env:  map[string]string{"RUNTIME_KEY": "runtime_value"},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {
				Runtime: "docker",
				Env:     map[string]string{"PROFILE_KEY": "profile_value"},
			},
		},
	}

	rtConfig, _, err := vs.ResolveRuntime("")
	require.NoError(t, err)
	assert.Equal(t, "runtime_value", rtConfig.Env["RUNTIME_KEY"], "runtime env should be preserved")
	assert.Equal(t, "profile_value", rtConfig.Env["PROFILE_KEY"], "profile env should be merged")
}

func TestResolveRuntime_ProfileNotFound(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "nonexistent",
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {Type: "docker"},
		},
		Profiles: map[string]V1ProfileConfig{},
	}

	_, _, err := vs.ResolveRuntime("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nonexistent")
}

func TestResolveRuntime_RuntimeNotFound(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "local",
		Runtimes:      map[string]V1RuntimeConfig{},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "missing-runtime"},
		},
	}

	_, _, err := vs.ResolveRuntime("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing-runtime")
}

// --- Hub helper method tests ---

func TestVersionedSettings_GetHubEndpoint(t *testing.T) {
	vs := &VersionedSettings{}
	assert.Equal(t, "", vs.GetHubEndpoint())

	vs.Hub = &V1HubClientConfig{Endpoint: "https://hub.example.com"}
	assert.Equal(t, "https://hub.example.com", vs.GetHubEndpoint())
}

func TestVersionedSettings_IsHubConfigured(t *testing.T) {
	vs := &VersionedSettings{}
	assert.False(t, vs.IsHubConfigured())

	vs.Hub = &V1HubClientConfig{}
	assert.False(t, vs.IsHubConfigured())

	vs.Hub.Endpoint = "https://hub.example.com"
	assert.True(t, vs.IsHubConfigured())
}

func TestVersionedSettings_IsHubEnabled(t *testing.T) {
	vs := &VersionedSettings{}
	assert.False(t, vs.IsHubEnabled())

	vs.Hub = &V1HubClientConfig{}
	assert.False(t, vs.IsHubEnabled())

	vs.Hub.Enabled = boolPtr(false)
	assert.False(t, vs.IsHubEnabled())

	vs.Hub.Enabled = boolPtr(true)
	assert.True(t, vs.IsHubEnabled())
}

func TestVersionedSettings_IsHubLinked(t *testing.T) {
	vs := &VersionedSettings{}
	assert.False(t, vs.IsHubLinked())

	vs.Hub = &V1HubClientConfig{}
	assert.False(t, vs.IsHubLinked())

	vs.Hub.Linked = boolPtr(false)
	assert.False(t, vs.IsHubLinked())

	vs.Hub.Linked = boolPtr(true)
	assert.True(t, vs.IsHubLinked())
}

func TestVersionedSettings_IsHubExplicitlyDisabled(t *testing.T) {
	vs := &VersionedSettings{}
	assert.False(t, vs.IsHubExplicitlyDisabled())

	vs.Hub = &V1HubClientConfig{Enabled: boolPtr(true)}
	assert.False(t, vs.IsHubExplicitlyDisabled())

	vs.Hub.Enabled = boolPtr(false)
	assert.True(t, vs.IsHubExplicitlyDisabled())
}

func TestVersionedSettings_IsHubLocalOnly(t *testing.T) {
	vs := &VersionedSettings{}
	assert.False(t, vs.IsHubLocalOnly())

	vs.Hub = &V1HubClientConfig{}
	assert.False(t, vs.IsHubLocalOnly())

	vs.Hub.LocalOnly = boolPtr(true)
	assert.True(t, vs.IsHubLocalOnly())
}

// --- Compatibility test ---

func TestLegacyAndVersionedResolution_SameResult(t *testing.T) {
	// Build legacy settings
	legacy := &Settings{
		ActiveProfile: "local",
		Runtimes: map[string]RuntimeConfig{
			"docker": {Host: "tcp://localhost:2375"},
		},
		Harnesses: map[string]HarnessConfig{
			"gemini": {
				Image: "example.com/gemini:latest",
				User:  "scion",
				Env:   map[string]string{"KEY1": "val1"},
				Volumes: []api.VolumeMount{
					{Source: "/host/path", Target: "/container/path"},
				},
			},
		},
		Profiles: map[string]ProfileConfig{
			"local": {
				Runtime: "docker",
				Env:     map[string]string{"PROFILE_KEY": "profile_val"},
				HarnessOverrides: map[string]HarnessOverride{
					"gemini": {
						Env: map[string]string{"OVERRIDE_KEY": "override_val"},
					},
				},
			},
		},
	}

	// Resolve via legacy path
	legacyHC, err := legacy.ResolveHarness("local", "gemini")
	require.NoError(t, err)

	// Adapt to versioned and resolve
	vs, _ := AdaptLegacySettings(legacy)
	versionedHC, err := vs.ResolveHarnessConfig("local", "gemini")
	require.NoError(t, err)

	// Compare results
	assert.Equal(t, legacyHC.Image, versionedHC.Image, "image should match")
	assert.Equal(t, legacyHC.User, versionedHC.User, "user should match")
	assert.Equal(t, legacyHC.Env["KEY1"], versionedHC.Env["KEY1"], "base env should match")
	assert.Equal(t, legacyHC.Env["PROFILE_KEY"], versionedHC.Env["PROFILE_KEY"], "profile env should match")
	assert.Equal(t, legacyHC.Env["OVERRIDE_KEY"], versionedHC.Env["OVERRIDE_KEY"], "override env should match")
	assert.Equal(t, len(legacyHC.Volumes), len(versionedHC.Volumes), "volume count should match")
}

// --- Phase 4: V1ServerConfig tests ---

func TestV1ServerConfig_YAMLRoundTrip(t *testing.T) {
	v1 := &V1ServerConfig{
		Env:       "production",
		LogLevel:  "debug",
		LogFormat: "json",
		Hub: &V1ServerHubConfig{
			Port:         9810,
			Host:         "0.0.0.0",
			HubID:        "test-hub-id",
			PublicURL:    "https://hub.example.com",
			ReadTimeout:  "30s",
			WriteTimeout: "60s",
			AdminEmails:  []string{"admin@example.com"},
			CORS: &V1CORSConfig{
				Enabled:        true,
				AllowedOrigins: []string{"*"},
				AllowedMethods: []string{"GET", "POST"},
				AllowedHeaders: []string{"Authorization"},
				MaxAge:         3600,
			},
		},
		Broker: &V1BrokerConfig{
			Enabled:        true,
			Port:           9800,
			Host:           "0.0.0.0",
			BrokerID:       "broker-123",
			BrokerName:     "my-broker",
			BrokerNickname: "broker-nick",
			BrokerToken:    "token-xyz",
			HubEndpoint:    "https://hub.example.com",
		},
		Database: &V1DatabaseConfig{
			Driver: "sqlite",
			URL:    "/tmp/hub.db",
		},
		Auth: &V1AuthConfig{
			DevMode:           true,
			DevToken:          "dev-token",
			AuthorizedDomains: []string{"example.com"},
		},
		Storage: &V1StorageConfig{
			Provider:  "local",
			LocalPath: "/tmp/storage",
		},
		Secrets: &V1SecretsConfig{
			Backend: "local",
		},
	}

	data, err := yaml.Marshal(v1)
	require.NoError(t, err)

	var roundTripped V1ServerConfig
	err = yaml.Unmarshal(data, &roundTripped)
	require.NoError(t, err)

	assert.Equal(t, v1.Env, roundTripped.Env)
	assert.Equal(t, v1.LogLevel, roundTripped.LogLevel)
	assert.Equal(t, v1.Hub.Port, roundTripped.Hub.Port)
	assert.Equal(t, v1.Hub.PublicURL, roundTripped.Hub.PublicURL)
	assert.Equal(t, v1.Broker.BrokerID, roundTripped.Broker.BrokerID)
	assert.Equal(t, v1.Broker.BrokerNickname, roundTripped.Broker.BrokerNickname)
	assert.Equal(t, v1.Database.Driver, roundTripped.Database.Driver)
	assert.Equal(t, v1.Auth.DevMode, roundTripped.Auth.DevMode)
	assert.Equal(t, v1.Storage.Provider, roundTripped.Storage.Provider)
}

func TestConvertV1ServerToGlobalConfig_Basic(t *testing.T) {
	v1 := &V1ServerConfig{
		LogLevel:  "debug",
		LogFormat: "json",
		Hub: &V1ServerHubConfig{
			Port:         9810,
			Host:         "0.0.0.0",
			HubID:        "test-hub-id",
			PublicURL:    "https://hub.example.com",
			ReadTimeout:  "30s",
			WriteTimeout: "60s",
			AdminEmails:  []string{"admin@example.com"},
			CORS: &V1CORSConfig{
				Enabled:        true,
				AllowedOrigins: []string{"*"},
				MaxAge:         3600,
			},
		},
		Broker: &V1BrokerConfig{
			Enabled:              true,
			Port:                 9800,
			BrokerID:             "broker-123",
			BrokerName:           "my-broker",
			BrokerNickname:       "nick",
			HubEndpoint:          "https://hub.example.com",
			ContainerHubEndpoint: "http://host.containers.internal:8080",
		},
		Database: &V1DatabaseConfig{
			Driver: "sqlite",
			URL:    "/tmp/hub.db",
		},
		Auth: &V1AuthConfig{
			DevMode:           true,
			DevToken:          "dev-token",
			AuthorizedDomains: []string{"example.com"},
		},
		Storage: &V1StorageConfig{
			Provider:  "local",
			LocalPath: "/tmp/storage",
		},
		Secrets: &V1SecretsConfig{
			Backend: "local",
		},
	}

	gc := ConvertV1ServerToGlobalConfig(v1)

	assert.Equal(t, "debug", gc.LogLevel)
	assert.Equal(t, "json", gc.LogFormat)
	assert.Equal(t, 9810, gc.Hub.Port)
	assert.Equal(t, "test-hub-id", gc.Hub.HubID)
	assert.Equal(t, "https://hub.example.com", gc.Hub.Endpoint)
	assert.Equal(t, true, gc.Hub.CORSEnabled)
	assert.Equal(t, 3600, gc.Hub.CORSMaxAge)
	assert.Equal(t, true, gc.RuntimeBroker.Enabled)
	assert.Equal(t, 9800, gc.RuntimeBroker.Port)
	assert.Equal(t, "broker-123", gc.RuntimeBroker.BrokerID)
	// BrokerName takes priority over BrokerNickname when both are set
	assert.Equal(t, "my-broker", gc.RuntimeBroker.BrokerName)
	assert.Equal(t, "https://hub.example.com", gc.RuntimeBroker.HubEndpoint)
	assert.Equal(t, "http://host.containers.internal:8080", gc.RuntimeBroker.ContainerHubEndpoint)
	assert.Equal(t, "sqlite", gc.Database.Driver)
	assert.Equal(t, "/tmp/hub.db", gc.Database.URL)
	assert.Equal(t, true, gc.Auth.Enabled)
	assert.Equal(t, "dev-token", gc.Auth.Token)
	assert.Equal(t, "local", gc.Storage.Provider)
	assert.Equal(t, "/tmp/storage", gc.Storage.LocalPath)
	assert.Equal(t, "local", gc.Secrets.Backend)
}

func TestConvertV1ServerToGlobalConfig_Mode(t *testing.T) {
	v1 := &V1ServerConfig{
		Mode: "hosted",
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, "hosted", gc.Mode)

	// Legacy "production" value should pass through (backward compat)
	v1Legacy := &V1ServerConfig{
		Mode: "production",
	}
	gcLegacy := ConvertV1ServerToGlobalConfig(v1Legacy)
	assert.Equal(t, "production", gcLegacy.Mode)

	// Empty mode should leave it empty (workstation default)
	v1Empty := &V1ServerConfig{}
	gcEmpty := ConvertV1ServerToGlobalConfig(v1Empty)
	assert.Equal(t, "", gcEmpty.Mode)
}

func TestConvertGlobalToV1ServerConfig_Mode(t *testing.T) {
	gc := DefaultGlobalConfig()
	gc.Mode = "hosted"

	v1 := ConvertGlobalToV1ServerConfig(&gc)
	assert.Equal(t, "hosted", v1.Mode)

	// Round-trip
	gc2 := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, "hosted", gc2.Mode)
}

func TestConvertV1ServerToGlobalConfig_Nil(t *testing.T) {
	gc := ConvertV1ServerToGlobalConfig(nil)
	assert.NotNil(t, gc)
	// Should be defaults
	assert.Equal(t, "info", gc.LogLevel)
}

func TestConvertV1ServerToGlobalConfig_AllowContainerScriptHarnesses(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	t.Run("no broker section defaults to true", func(t *testing.T) {
		v1 := &V1ServerConfig{}
		gc := ConvertV1ServerToGlobalConfig(v1)
		assert.True(t, gc.RuntimeBroker.AllowContainerScriptHarnesses)
	})

	t.Run("broker section without field defaults to true", func(t *testing.T) {
		v1 := &V1ServerConfig{
			Broker: &V1BrokerConfig{Enabled: true},
		}
		gc := ConvertV1ServerToGlobalConfig(v1)
		assert.True(t, gc.RuntimeBroker.AllowContainerScriptHarnesses)
	})

	t.Run("broker section with explicit false", func(t *testing.T) {
		v1 := &V1ServerConfig{
			Broker: &V1BrokerConfig{
				AllowContainerScriptHarnesses: boolPtr(false),
			},
		}
		gc := ConvertV1ServerToGlobalConfig(v1)
		assert.False(t, gc.RuntimeBroker.AllowContainerScriptHarnesses)
	})

	t.Run("broker section with explicit true", func(t *testing.T) {
		v1 := &V1ServerConfig{
			Broker: &V1BrokerConfig{
				AllowContainerScriptHarnesses: boolPtr(true),
			},
		}
		gc := ConvertV1ServerToGlobalConfig(v1)
		assert.True(t, gc.RuntimeBroker.AllowContainerScriptHarnesses)
	})

	t.Run("nil config defaults to true", func(t *testing.T) {
		gc := ConvertV1ServerToGlobalConfig(nil)
		assert.True(t, gc.RuntimeBroker.AllowContainerScriptHarnesses)
	})
}

func TestConvertGlobalToV1ServerConfig_RoundTrip(t *testing.T) {
	gc := DefaultGlobalConfig()
	gc.LogLevel = "debug"
	gc.Hub.Port = 9999
	gc.Hub.HubID = "roundtrip-hub-id"
	gc.RuntimeBroker.Enabled = true
	gc.RuntimeBroker.BrokerID = "broker-abc"
	gc.RuntimeBroker.BrokerName = "test-broker"
	gc.Database.Driver = "sqlite"
	gc.Auth.Enabled = true
	gc.Auth.Token = "test-token"

	v1 := ConvertGlobalToV1ServerConfig(&gc)

	assert.Equal(t, "debug", v1.LogLevel)
	assert.Equal(t, 9999, v1.Hub.Port)
	assert.Equal(t, "roundtrip-hub-id", v1.Hub.HubID)
	assert.Equal(t, true, v1.Broker.Enabled)
	assert.Equal(t, "broker-abc", v1.Broker.BrokerID)
	assert.Equal(t, "test-broker", v1.Broker.BrokerName)
	assert.Equal(t, "sqlite", v1.Database.Driver)
	assert.Equal(t, true, v1.Auth.DevMode)
	assert.Equal(t, "test-token", v1.Auth.DevToken)

	// Round-trip back
	gc2 := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, gc.LogLevel, gc2.LogLevel)
	assert.Equal(t, gc.Hub.Port, gc2.Hub.Port)
	assert.Equal(t, gc.Hub.HubID, gc2.Hub.HubID)
	assert.Equal(t, gc.RuntimeBroker.Enabled, gc2.RuntimeBroker.Enabled)
	assert.Equal(t, gc.RuntimeBroker.BrokerID, gc2.RuntimeBroker.BrokerID)
	assert.Equal(t, gc.RuntimeBroker.BrokerName, gc2.RuntimeBroker.BrokerName)
}

func TestConvertGlobalToV1ServerConfig_Nil(t *testing.T) {
	v1 := ConvertGlobalToV1ServerConfig(nil)
	assert.NotNil(t, v1)
}

func TestLoadGlobalConfig_FromSettingsYAML(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	// Write settings.yaml with server key
	settingsContent := `
schema_version: "1"
server:
  log_level: debug
  log_format: json
  hub:
    port: 9999
    host: "0.0.0.0"
    hub_id: "settings-hub-id"
  broker:
    enabled: true
    port: 8888
    broker_id: "test-broker-id"
    broker_nickname: "test-broker-nick"
  database:
    driver: sqlite
  auth:
    dev_mode: true
    dev_token: "test-dev-token"
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)

	assert.Equal(t, "debug", gc.LogLevel)
	assert.Equal(t, "json", gc.LogFormat)
	assert.Equal(t, 9999, gc.Hub.Port)
	assert.Equal(t, "settings-hub-id", gc.Hub.HubID)
	assert.Equal(t, true, gc.RuntimeBroker.Enabled)
	assert.Equal(t, 8888, gc.RuntimeBroker.Port)
	assert.Equal(t, "test-broker-id", gc.RuntimeBroker.BrokerID)
	assert.Equal(t, "test-broker-nick", gc.RuntimeBroker.BrokerName)
	assert.Equal(t, true, gc.Auth.Enabled)
	assert.Equal(t, "test-dev-token", gc.Auth.Token)
}

func TestLoadGlobalConfig_TelemetryEnabledFromSettingsYAML(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	// Write settings.yaml with top-level telemetry section (separate from server)
	settingsContent := `
schema_version: "1"
server:
  mode: production
  broker:
    broker_id: "test-broker-id"
telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "cloudtrace.googleapis.com:443"
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)

	require.NotNil(t, gc.TelemetryEnabled, "TelemetryEnabled should be populated from top-level telemetry.enabled")
	assert.True(t, *gc.TelemetryEnabled)
}

func TestLoadGlobalConfig_TelemetryDisabledFromSettingsYAML(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	settingsContent := `
schema_version: "1"
server:
  mode: production
telemetry:
  enabled: false
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)

	require.NotNil(t, gc.TelemetryEnabled)
	assert.False(t, *gc.TelemetryEnabled)
}

func TestLoadGlobalConfig_NoTelemetrySectionLeavesNil(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	settingsContent := `
schema_version: "1"
server:
  mode: production
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)

	assert.Nil(t, gc.TelemetryEnabled, "TelemetryEnabled should be nil when no telemetry section exists")
}

func TestLoadGlobalConfig_SettingsYAMLPreferredOverServerYAML(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	// Write settings.yaml with server key
	settingsContent := `
schema_version: "1"
server:
  log_level: debug
  hub:
    port: 9999
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	// Write server.yaml (legacy) — should NOT be used
	serverContent := `
logLevel: warn
hub:
  port: 1111
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte(serverContent), 0644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)

	// settings.yaml should win
	assert.Equal(t, "debug", gc.LogLevel)
	assert.Equal(t, 9999, gc.Hub.Port)
}

func TestLoadGlobalConfig_FallsBackToServerYAML(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	// Write settings.yaml WITHOUT server key
	settingsContent := `
schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	// Write server.yaml
	serverContent := `
logLevel: warn
hub:
  port: 7777
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte(serverContent), 0644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)

	// server.yaml should be used
	assert.Equal(t, "warn", gc.LogLevel)
	assert.Equal(t, 7777, gc.Hub.Port)
}

func TestAdaptLegacySettings_PopulatesServerBroker(t *testing.T) {
	legacy := &Settings{
		Hub: &HubClientConfig{
			BrokerID:       "broker-123",
			BrokerNickname: "my-broker",
			BrokerToken:    "broker-token",
		},
	}

	vs, warnings := AdaptLegacySettings(legacy)

	// Server.Broker should be populated
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, "broker-123", vs.Server.Broker.BrokerID)
	assert.Equal(t, "my-broker", vs.Server.Broker.BrokerNickname)
	assert.Equal(t, "broker-token", vs.Server.Broker.BrokerToken)

	// Should have deprecation warnings
	assert.NotEmpty(t, warnings)
	warningTexts := map[string]bool{
		"hub.brokerId":       false,
		"hub.brokerNickname": false,
		"hub.brokerToken":    false,
	}
	for _, w := range warnings {
		for key := range warningTexts {
			if strings.Contains(w, key) {
				warningTexts[key] = true
			}
		}
	}
	for key, found := range warningTexts {
		assert.True(t, found, "expected warning about %s", key)
	}
}

func TestVersionedEnvKeyMapper_DeepServerNesting(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		// Basic server keys
		{"SCION_SERVER_ENV", "server.env"},
		{"SCION_SERVER_LOG_LEVEL", "server.log_level"},
		{"SCION_SERVER_LOG_FORMAT", "server.log_format"},
		// Hub server keys
		{"SCION_SERVER_HUB_PORT", "server.hub.port"},
		{"SCION_SERVER_HUB_HOST", "server.hub.host"},
		{"SCION_SERVER_HUB_PUBLIC_URL", "server.hub.public_url"},
		{"SCION_SERVER_HUB_READ_TIMEOUT", "server.hub.read_timeout"},
		{"SCION_SERVER_HUB_WRITE_TIMEOUT", "server.hub.write_timeout"},
		// Broker keys
		{"SCION_SERVER_BROKER_PORT", "server.broker.port"},
		{"SCION_SERVER_BROKER_HOST", "server.broker.host"},
		{"SCION_SERVER_BROKER_BROKER_ID", "server.broker.broker_id"},
		{"SCION_SERVER_BROKER_BROKER_NAME", "server.broker.broker_name"},
		{"SCION_SERVER_BROKER_BROKER_NICKNAME", "server.broker.broker_nickname"},
		{"SCION_SERVER_BROKER_BROKER_TOKEN", "server.broker.broker_token"},
		{"SCION_SERVER_BROKER_HUB_ENDPOINT", "server.broker.hub_endpoint"},
		// Database keys
		{"SCION_SERVER_DATABASE_DRIVER", "server.database.driver"},
		{"SCION_SERVER_DATABASE_URL", "server.database.url"},
		// Auth keys
		{"SCION_SERVER_AUTH_DEV_MODE", "server.auth.dev_mode"},
		{"SCION_SERVER_AUTH_DEV_TOKEN", "server.auth.dev_token"},
		{"SCION_SERVER_AUTH_DEV_TOKEN_FILE", "server.auth.dev_token_file"},
		{"SCION_SERVER_AUTH_AUTHORIZED_DOMAINS", "server.auth.authorized_domains"},
		// OAuth keys
		{"SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENT_ID", "server.oauth.web.google.client_id"},
		{"SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENT_SECRET", "server.oauth.web.google.client_secret"},
		{"SCION_SERVER_OAUTH_CLI_GITHUB_CLIENT_ID", "server.oauth.cli.github.client_id"},
		// Storage keys
		{"SCION_SERVER_STORAGE_PROVIDER", "server.storage.provider"},
		{"SCION_SERVER_STORAGE_LOCAL_PATH", "server.storage.local_path"},
		// Secrets keys
		{"SCION_SERVER_SECRETS_BACKEND", "server.secrets.backend"},
		{"SCION_SERVER_SECRETS_GCP_PROJECT_ID", "server.secrets.gcp_project_id"},
		{"SCION_SERVER_SECRETS_GCP_CREDENTIALS", "server.secrets.gcp_credentials"},
		// CORS keys (nested under hub or broker)
		{"SCION_SERVER_HUB_CORS_ENABLED", "server.hub.cors.enabled"},
		{"SCION_SERVER_HUB_CORS_ALLOWED_ORIGINS", "server.hub.cors.allowed_origins"},
		{"SCION_SERVER_HUB_CORS_MAX_AGE", "server.hub.cors.max_age"},
		{"SCION_SERVER_BROKER_CORS_ENABLED", "server.broker.cors.enabled"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := versionedEnvKeyMapper(tt.input)
			assert.Equal(t, tt.expected, result, "input: %s", tt.input)
		})
	}
}

func TestMergeServerIntoSettings(t *testing.T) {
	tmpDir := t.TempDir()

	// Write existing settings.yaml
	existingContent := `
schema_version: "1"
active_profile: local
default_template: gemini
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(existingContent), 0644))

	v1 := &V1ServerConfig{
		LogLevel: "debug",
		Hub: &V1ServerHubConfig{
			Port: 9999,
		},
	}

	err := MergeServerIntoSettings(tmpDir, v1)
	require.NoError(t, err)

	// Re-read and verify
	data, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "server:")
	assert.Contains(t, content, "active_profile")
	assert.Contains(t, content, "schema_version")
}

// --- Phase 6: Migration tests ---

func TestSaveVersionedSettings(t *testing.T) {
	tmpDir := t.TempDir()

	vs := &VersionedSettings{
		SchemaVersion:   "1",
		ActiveProfile:   "local",
		DefaultTemplate: "gemini",
		Hub: &V1HubClientConfig{
			Enabled:  boolPtr(true),
			Endpoint: "https://hub.example.com",
		},
		HarnessConfigs: map[string]HarnessConfigEntry{
			"gemini": {
				Harness: "gemini",
				Image:   "example.com/gemini:latest",
				User:    "scion",
			},
		},
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {Type: "docker"},
		},
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
	}

	err := SaveVersionedSettings(tmpDir, vs)
	require.NoError(t, err)

	// Verify file exists
	data, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	require.NoError(t, err)

	// Load it back
	var loaded VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &loaded))

	assert.Equal(t, "1", loaded.SchemaVersion)
	assert.Equal(t, "local", loaded.ActiveProfile)
	assert.Equal(t, "gemini", loaded.DefaultTemplate)
	assert.Equal(t, "https://hub.example.com", loaded.Hub.Endpoint)
	assert.Equal(t, "gemini", loaded.HarnessConfigs["gemini"].Harness)
	assert.Equal(t, "docker", loaded.Runtimes["docker"].Type)
	assert.Equal(t, "docker", loaded.Profiles["local"].Runtime)
}

func TestMigrateSettingsFile_LegacyYAML(t *testing.T) {
	tmpDir := t.TempDir()

	legacyContent := `
active_profile: local
default_template: gemini
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
  claude:
    image: example.com/claude:latest
    user: scion
runtimes:
  docker:
    host: tcp://localhost:2375
profiles:
  local:
    runtime: docker
hub:
  endpoint: https://hub.example.com
  groveId: test-grove
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(legacyContent), 0644))

	result, err := MigrateSettingsFile(tmpDir, false)
	require.NoError(t, err)

	assert.False(t, result.Skipped)
	assert.Equal(t, "legacy", result.Format)
	assert.False(t, result.WasJSON)
	assert.NotEmpty(t, result.BackupPath)
	assert.Contains(t, result.BackupPath, ".bak")

	// Verify backup exists
	_, err = os.Stat(result.BackupPath)
	assert.NoError(t, err)

	// Verify new file is versioned
	newData, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(newData)
	assert.Equal(t, "1", version)

	// Verify harnesses warning is present
	hasHarnessWarning := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "harnesses is deprecated") {
			hasHarnessWarning = true
			break
		}
	}
	assert.True(t, hasHarnessWarning)
}

func TestMigrateSettingsFile_LegacyJSON(t *testing.T) {
	tmpDir := t.TempDir()

	legacyJSON := `{
		"active_profile": "local",
		"default_template": "gemini",
		"harnesses": {
			"gemini": {
				"image": "example.com/gemini:latest",
				"user": "scion"
			}
		},
		"runtimes": {
			"docker": {}
		},
		"profiles": {
			"local": {
				"runtime": "docker"
			}
		}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.json"), []byte(legacyJSON), 0644))

	result, err := MigrateSettingsFile(tmpDir, false)
	require.NoError(t, err)

	assert.False(t, result.Skipped)
	assert.True(t, result.WasJSON)
	assert.Contains(t, result.BackupPath, ".json.bak")

	// Output should be .yaml
	_, err = os.Stat(filepath.Join(tmpDir, "settings.yaml"))
	assert.NoError(t, err)

	// Verify it's versioned
	newData, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	require.NoError(t, err)
	version, _ := DetectSettingsFormat(newData)
	assert.Equal(t, "1", version)
}

func TestMigrateSettingsFile_AlreadyVersioned(t *testing.T) {
	tmpDir := t.TempDir()

	versionedContent := `
schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(versionedContent), 0644))

	result, err := MigrateSettingsFile(tmpDir, false)
	require.NoError(t, err)

	assert.True(t, result.Skipped)
	assert.Equal(t, "versioned", result.Format)
	assert.Contains(t, result.SkipReason, "already versioned")

	// No backup should be created
	assert.Empty(t, result.BackupPath)
}

func TestMigrateSettingsFile_NoFile(t *testing.T) {
	tmpDir := t.TempDir()

	result, err := MigrateSettingsFile(tmpDir, false)
	require.NoError(t, err)

	assert.True(t, result.Skipped)
	assert.Equal(t, "no settings file found", result.SkipReason)
}

func TestMigrateSettingsFile_DryRun(t *testing.T) {
	tmpDir := t.TempDir()

	legacyContent := `
active_profile: local
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
runtimes:
  docker: {}
profiles:
  local:
    runtime: docker
`
	settingsPath := filepath.Join(tmpDir, "settings.yaml")
	require.NoError(t, os.WriteFile(settingsPath, []byte(legacyContent), 0644))

	result, err := MigrateSettingsFile(tmpDir, true)
	require.NoError(t, err)

	assert.False(t, result.Skipped)
	assert.Equal(t, "legacy", result.Format)
	assert.NotEmpty(t, result.Warnings)
	assert.Empty(t, result.BackupPath) // dry run — no backup created

	// Original file should be unchanged
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	version, _ := DetectSettingsFormat(data)
	assert.Empty(t, version, "original file should still be legacy")
}

func TestMigrateSettingsFile_LastSyncedAt(t *testing.T) {
	tmpDir := t.TempDir()

	legacyContent := `
active_profile: local
hub:
  endpoint: https://hub.example.com
  lastSyncedAt: "2024-06-15T10:30:00Z"
runtimes:
  docker: {}
profiles:
  local:
    runtime: docker
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(legacyContent), 0644))

	result, err := MigrateSettingsFile(tmpDir, false)
	require.NoError(t, err)

	assert.True(t, result.StateMigrated)

	// Verify state.yaml was created with the timestamp
	state, err := LoadProjectState(tmpDir)
	require.NoError(t, err)
	assert.Equal(t, "2024-06-15T10:30:00Z", state.LastSyncedAt)
}

func TestMigrateSettingsFile_BackupExists(t *testing.T) {
	tmpDir := t.TempDir()

	legacyContent := `
active_profile: local
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
runtimes:
  docker: {}
profiles:
  local:
    runtime: docker
`
	settingsPath := filepath.Join(tmpDir, "settings.yaml")

	// Create existing .bak and .bak.1 files
	require.NoError(t, os.WriteFile(settingsPath+".bak", []byte("old backup"), 0644))
	require.NoError(t, os.WriteFile(settingsPath+".bak.1", []byte("old backup 1"), 0644))
	require.NoError(t, os.WriteFile(settingsPath, []byte(legacyContent), 0644))

	result, err := MigrateSettingsFile(tmpDir, false)
	require.NoError(t, err)

	assert.False(t, result.Skipped)
	// Should use .bak.2 since .bak and .bak.1 exist
	assert.Equal(t, settingsPath+".bak.2", result.BackupPath)
	_, err = os.Stat(result.BackupPath)
	assert.NoError(t, err)
}

func TestMigrateSettingsFile_ValidationPass(t *testing.T) {
	tmpDir := t.TempDir()

	legacyContent := `
active_profile: local
default_template: gemini
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
runtimes:
  docker: {}
profiles:
  local:
    runtime: docker
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(legacyContent), 0644))

	result, err := MigrateSettingsFile(tmpDir, false)
	require.NoError(t, err)
	assert.False(t, result.Skipped)

	// Read the migrated file and validate against schema
	data, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	require.NoError(t, err)

	valErrors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, valErrors, "migrated file should validate against v1 schema: %v", valErrors)
}

func TestMigrateSettingsFile_DeprecationWarnings(t *testing.T) {
	tmpDir := t.TempDir()

	legacyContent := `
active_profile: local
hub:
  token: secret
  apiKey: api-key
  brokerId: broker-123
  brokerNickname: my-broker
  brokerToken: broker-token
  lastSyncedAt: "2024-01-01T00:00:00Z"
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
bucket:
  provider: GCS
  name: my-bucket
runtimes:
  docker: {}
profiles:
  local:
    runtime: docker
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(legacyContent), 0644))

	result, err := MigrateSettingsFile(tmpDir, true)
	require.NoError(t, err)

	// Check all expected warnings are present
	expectedWarnings := []string{
		"hub.token",
		"hub.apiKey",
		"hub.brokerId",
		"hub.brokerNickname",
		"hub.brokerToken",
		"hub.lastSyncedAt",
		"harnesses is deprecated",
		"bucket config is deprecated",
	}

	for _, expected := range expectedWarnings {
		found := false
		for _, w := range result.Warnings {
			if strings.Contains(w, expected) {
				found = true
				break
			}
		}
		assert.True(t, found, "expected warning containing %q", expected)
	}
}

func TestMigrateSettingsFile_WithServerYAML(t *testing.T) {
	tmpDir := t.TempDir()

	legacySettings := `
active_profile: local
default_template: gemini
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
runtimes:
  docker: {}
profiles:
  local:
    runtime: docker
`
	serverYAML := `
hub:
  port: 9810
  host: "0.0.0.0"
runtimeBroker:
  enabled: true
  port: 9800
database:
  driver: sqlite
auth:
  devMode: true
  devToken: test-token
logLevel: debug
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(legacySettings), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "server.yaml"), []byte(serverYAML), 0644))

	result, err := MigrateSettingsFile(tmpDir, false)
	require.NoError(t, err)

	assert.False(t, result.Skipped)
	assert.Equal(t, "legacy", result.Format)
	assert.True(t, result.ServerMigrated)
	assert.NotEmpty(t, result.ServerBackupPath)
	assert.Contains(t, result.ServerBackupPath, "server.yaml.bak")

	// Verify server.yaml was backed up (moved away)
	_, err = os.Stat(filepath.Join(tmpDir, "server.yaml"))
	assert.True(t, os.IsNotExist(err), "server.yaml should have been moved to backup")
	_, err = os.Stat(result.ServerBackupPath)
	assert.NoError(t, err, "server.yaml backup should exist")

	// Read the migrated settings and verify server config is present
	newData, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(newData)
	assert.Equal(t, "1", version)

	// Parse and verify server section is populated
	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(newData, &vs))
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Hub)
	assert.Equal(t, 9810, vs.Server.Hub.Port)
	require.NotNil(t, vs.Server.Broker)
	assert.True(t, vs.Server.Broker.Enabled)
	assert.Equal(t, 9800, vs.Server.Broker.Port)
	require.NotNil(t, vs.Server.Auth)
	assert.True(t, vs.Server.Auth.DevMode)
	assert.Equal(t, "debug", vs.Server.LogLevel)
}

func TestMigrateSettingsFile_ServerYAML_BrokerIdentityMerge(t *testing.T) {
	tmpDir := t.TempDir()

	// Legacy settings has broker identity in hub section
	legacySettings := `
active_profile: local
hub:
  endpoint: https://hub.example.com
  brokerId: legacy-broker-id
  brokerNickname: my-broker
  brokerToken: legacy-token
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
runtimes:
  docker: {}
profiles:
  local:
    runtime: docker
`
	// server.yaml has broker section but without identity fields
	serverYAML := `
hub:
  port: 9810
runtimeBroker:
  enabled: true
  port: 9800
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(legacySettings), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "server.yaml"), []byte(serverYAML), 0644))

	result, err := MigrateSettingsFile(tmpDir, false)
	require.NoError(t, err)

	assert.True(t, result.ServerMigrated)

	// Parse migrated settings
	newData, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	require.NoError(t, err)

	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(newData, &vs))

	// Broker identity from legacy hub should be preserved since server.yaml didn't have them
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, "legacy-broker-id", vs.Server.Broker.BrokerID)
	assert.Equal(t, "my-broker", vs.Server.Broker.BrokerNickname)
	assert.Equal(t, "legacy-token", vs.Server.Broker.BrokerToken)
	// Server.yaml values should also be present
	assert.True(t, vs.Server.Broker.Enabled)
	assert.Equal(t, 9800, vs.Server.Broker.Port)
}

func TestMigrateSettingsFile_ServerYAML_DryRun(t *testing.T) {
	tmpDir := t.TempDir()

	legacySettings := `
active_profile: local
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
runtimes:
  docker: {}
profiles:
  local:
    runtime: docker
`
	serverYAML := `
hub:
  port: 9810
logLevel: info
`
	settingsPath := filepath.Join(tmpDir, "settings.yaml")
	serverPath := filepath.Join(tmpDir, "server.yaml")
	require.NoError(t, os.WriteFile(settingsPath, []byte(legacySettings), 0644))
	require.NoError(t, os.WriteFile(serverPath, []byte(serverYAML), 0644))

	result, err := MigrateSettingsFile(tmpDir, true)
	require.NoError(t, err)

	assert.False(t, result.Skipped)
	assert.True(t, result.ServerMigrated)
	// Dry run should not create backups
	assert.Empty(t, result.BackupPath)
	assert.Empty(t, result.ServerBackupPath)

	// Original files should be unchanged
	_, err = os.Stat(settingsPath)
	assert.NoError(t, err)
	_, err = os.Stat(serverPath)
	assert.NoError(t, err)

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	version, _ := DetectSettingsFormat(data)
	assert.Empty(t, version, "original settings should still be legacy")
}

func TestMigrateSettingsFile_NoServerYAML(t *testing.T) {
	// When no server.yaml exists, migration should work normally without server merging
	tmpDir := t.TempDir()

	legacySettings := `
active_profile: local
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
runtimes:
  docker: {}
profiles:
  local:
    runtime: docker
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(legacySettings), 0644))

	result, err := MigrateSettingsFile(tmpDir, false)
	require.NoError(t, err)

	assert.False(t, result.Skipped)
	assert.False(t, result.ServerMigrated)
	assert.Empty(t, result.ServerBackupPath)
}

func TestMigrateSettingsFile_RealWorldExample(t *testing.T) {
	// Test with the real-world legacy settings example from the design doc
	tmpDir := t.TempDir()

	realWorldSettings := `active_profile: local
default_template: gemini
hub:
    enabled: false
    endpoint: http://localhost:9810
    brokerId: 5e738c37-e6a2-463f-b2fc-3a742db7ec6d
cli:
    autohelp: true
runtimes:
    container:
        tmux: true
    docker: {}
    kubernetes: {}
harnesses:
    claude:
        image: us-central1-docker.pkg.dev/example-project/scion-images/scion-claude:latest
        user: scion
    codex:
        image: us-central1-docker.pkg.dev/example-project/scion-images/scion-codex:latest
        user: scion
    gemini:
        image: us-central1-docker.pkg.dev/example-project/scion-images/scion-gemini:latest
        user: scion
    opencode:
        image: us-central1-docker.pkg.dev/example-project/scion-images/scion-opencode:latest
        user: scion
profiles:
    local:
        runtime: container
        tmux: true
        env:
            GIT_AUTHOR_EMAIL: dev@example.com
            GIT_AUTHOR_NAME: Test User
            GIT_COMMITTER_EMAIL: dev@example.com
            GIT_COMMITTER_NAME: Test User
        volumes:
            - source: ${GOPATH}/pkg
              target: /home/scion/go/pkg
            - source: /home/dev/.cache/go-build
              target: /home/scion/.cache/go-build
    remote:
        runtime: kubernetes
        tmux: true
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(realWorldSettings), 0644))

	result, err := MigrateSettingsFile(tmpDir, false)
	require.NoError(t, err)

	assert.False(t, result.Skipped)
	assert.Equal(t, "legacy", result.Format)

	// Read and verify the migrated file
	newData, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(newData)
	assert.Equal(t, "1", version, "migrated file should be versioned")

	// Validate against schema
	valErrors, err := ValidateSettings(newData, "1")
	require.NoError(t, err)
	assert.Empty(t, valErrors, "migrated file should validate against v1 schema: %v", valErrors)

	// Parse and verify key fields
	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(newData, &vs))

	assert.Equal(t, "1", vs.SchemaVersion)
	assert.Equal(t, "local", vs.ActiveProfile)
	assert.Equal(t, "gemini", vs.DefaultTemplate)

	// Hub
	require.NotNil(t, vs.Hub)
	require.NotNil(t, vs.Hub.Enabled)
	assert.False(t, *vs.Hub.Enabled)
	assert.Equal(t, "http://localhost:9810", vs.Hub.Endpoint)

	// BrokerId should have moved to server.broker
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, "5e738c37-e6a2-463f-b2fc-3a742db7ec6d", vs.Server.Broker.BrokerID)

	// CLI
	require.NotNil(t, vs.CLI)
	require.NotNil(t, vs.CLI.AutoHelp)
	assert.True(t, *vs.CLI.AutoHelp)

	// Runtimes — should have type field set from key
	assert.Len(t, vs.Runtimes, 3)
	assert.Equal(t, "container", vs.Runtimes["container"].Type)
	assert.Equal(t, "docker", vs.Runtimes["docker"].Type)
	assert.Equal(t, "kubernetes", vs.Runtimes["kubernetes"].Type)

	// HarnessConfigs — should be renamed from harnesses
	assert.Len(t, vs.HarnessConfigs, 4)
	assert.Equal(t, "claude", vs.HarnessConfigs["claude"].Harness)
	assert.Equal(t, "gemini", vs.HarnessConfigs["gemini"].Harness)
	assert.Equal(t, "codex", vs.HarnessConfigs["codex"].Harness)
	assert.Equal(t, "opencode", vs.HarnessConfigs["opencode"].Harness)
	assert.Contains(t, vs.HarnessConfigs["gemini"].Image, "scion-gemini")

	// Profiles
	assert.Len(t, vs.Profiles, 2)
	assert.Equal(t, "container", vs.Profiles["local"].Runtime)
	assert.Equal(t, "kubernetes", vs.Profiles["remote"].Runtime)
	assert.Equal(t, "dev@example.com", vs.Profiles["local"].Env["GIT_AUTHOR_EMAIL"])
	assert.Len(t, vs.Profiles["local"].Volumes, 2)

	// Check deprecation warnings
	hasHarnessWarning := false
	hasBrokerWarning := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "harnesses is deprecated") {
			hasHarnessWarning = true
		}
		if strings.Contains(w, "hub.brokerId") {
			hasBrokerWarning = true
		}
	}
	assert.True(t, hasHarnessWarning, "should warn about harnesses rename")
	assert.True(t, hasBrokerWarning, "should warn about brokerId move")
}

func TestMigrateSettingsFile_HarnessOverrideAuthSelectedType(t *testing.T) {
	// Regression test: legacy auth_selectedType (camelCase) must be migrated to
	// auth_selected_type (snake_case) and pass schema validation.
	tmpDir := t.TempDir()

	legacyContent := `
grove_id: github.com/example/project
active_profile: local
default_template: claude
hub:
    enabled: false
cli:
    autohelp: false
runtimes:
    container:
        tmux: true
    docker: {}
    kubernetes: {}
harnesses:
    claude:
        image: example.com/scion-claude:latest
        user: scion
    gemini:
        image: example.com/scion-gemini:latest
        user: scion
profiles:
    local:
        runtime: container
        tmux: true
        harness_overrides:
            gemini:
                auth_selectedType: auth-file
    remote:
        runtime: kubernetes
        tmux: true
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(legacyContent), 0644))

	result, err := MigrateSettingsFile(tmpDir, false)
	require.NoError(t, err)

	assert.False(t, result.Skipped)
	assert.Equal(t, "legacy", result.Format)

	// Read the migrated file
	newData, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	require.NoError(t, err)

	// Must validate against the schema
	valErrors, err := ValidateSettings(newData, "1")
	require.NoError(t, err)
	assert.Empty(t, valErrors, "migrated file should pass schema validation: %v", valErrors)

	// Verify the field was written as snake_case
	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(newData, &vs))

	require.Contains(t, vs.Profiles, "local")
	require.Contains(t, vs.Profiles["local"].HarnessOverrides, "gemini")
	assert.Equal(t, "auth-file", vs.Profiles["local"].HarnessOverrides["gemini"].AuthSelectedType)

	// Also verify the raw YAML contains snake_case, not camelCase
	assert.Contains(t, string(newData), "auth_selected_type")
	assert.NotContains(t, string(newData), "auth_selectedType")
}

// --- UpdateSetting v1 format preservation tests ---

func TestUpdateSetting_PreservesV1Format(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Write a v1 versioned settings file
	v1Content := `schema_version: "1"
active_profile: local
hub:
  enabled: true
  endpoint: https://hub.example.com
  grove_id: original-grove-id
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(v1Content), 0644))

	// Call UpdateSetting with a key that would clobber the format in the old code
	err := UpdateSetting(projectDir, "grove_id", "new-grove-id", false)
	require.NoError(t, err)

	// Read back the file and verify it's still v1 format
	data, err := os.ReadFile(filepath.Join(projectDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(data)
	assert.Equal(t, "1", version, "file should still be v1 format after UpdateSetting")

	// Verify the field was updated
	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))
	assert.Equal(t, "new-grove-id", vs.Hub.ProjectID)

	// Verify other fields are preserved
	assert.Equal(t, "local", vs.ActiveProfile)
	require.NotNil(t, vs.Hub)
	require.NotNil(t, vs.Hub.Enabled)
	assert.True(t, *vs.Hub.Enabled)
	assert.Equal(t, "https://hub.example.com", vs.Hub.Endpoint)
}

func TestUpdateSetting_V1HubEnabled(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	v1Content := `schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(v1Content), 0644))

	err := UpdateSetting(projectDir, "hub.enabled", "true", false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(projectDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(data)
	assert.Equal(t, "1", version)

	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))
	require.NotNil(t, vs.Hub)
	require.NotNil(t, vs.Hub.Enabled)
	assert.True(t, *vs.Hub.Enabled)
}

func TestUpdateSetting_V1BrokerIdMapsToServerBroker(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	v1Content := `schema_version: "1"
active_profile: local
hub:
  endpoint: https://hub.example.com
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(v1Content), 0644))

	// Update hub.brokerId — in v1 this maps to server.broker.broker_id
	err := UpdateSetting(projectDir, "hub.brokerId", "broker-123", false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(projectDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(data)
	assert.Equal(t, "1", version)

	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))

	// Should be in server.broker, not hub
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, "broker-123", vs.Server.Broker.BrokerID)

	// Hub should still be intact
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "https://hub.example.com", vs.Hub.Endpoint)
}

func TestUpdateSetting_V1BrokerTokenMapsToServerBroker(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	v1Content := `schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(v1Content), 0644))

	err := UpdateSetting(projectDir, "hub.brokerToken", "token-xyz", false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(projectDir, "settings.yaml"))
	require.NoError(t, err)

	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))

	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, "token-xyz", vs.Server.Broker.BrokerToken)
}

func TestUpdateSetting_V1DeprecatedKeysSkipSilently(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	v1Content := `schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(v1Content), 0644))

	// These keys are deprecated in v1 and should be silently skipped
	for _, key := range []string{"hub.token", "hub.apiKey", "hub.lastSyncedAt"} {
		err := UpdateSetting(projectDir, key, "some-value", false)
		assert.NoError(t, err, "deprecated key %s should not error", key)
	}

	// File should be unchanged (besides schema_version being preserved)
	data, err := os.ReadFile(filepath.Join(projectDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(data)
	assert.Equal(t, "1", version)
}

func TestUpdateSetting_V1MultipleUpdatesPreserveFormat(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	v1Content := `schema_version: "1"
active_profile: local
default_template: gemini
hub:
  enabled: true
  endpoint: https://hub.example.com
  grove_id: grove-1
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(v1Content), 0644))

	// Simulate what happens during hub operations: multiple sequential updates
	require.NoError(t, UpdateSetting(projectDir, "grove_id", "new-grove-id", false))
	require.NoError(t, UpdateSetting(projectDir, "hub.brokerId", "broker-abc", false))
	require.NoError(t, UpdateSetting(projectDir, "hub.brokerToken", "token-xyz", false))
	require.NoError(t, UpdateSetting(projectDir, "hub.enabled", "false", false))

	// Read final state
	data, err := os.ReadFile(filepath.Join(projectDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(data)
	assert.Equal(t, "1", version, "file should still be v1 after multiple updates")

	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))

	// Verify all updates took effect
	assert.Equal(t, "new-grove-id", vs.Hub.ProjectID)
	require.NotNil(t, vs.Hub.Enabled)
	assert.False(t, *vs.Hub.Enabled)
	assert.Equal(t, "https://hub.example.com", vs.Hub.Endpoint) // preserved

	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, "broker-abc", vs.Server.Broker.BrokerID)
	assert.Equal(t, "token-xyz", vs.Server.Broker.BrokerToken)

	// Original fields should be preserved
	assert.Equal(t, "local", vs.ActiveProfile)
	assert.Equal(t, "gemini", vs.DefaultTemplate)
}

func TestUpdateSetting_LegacyFormatAutoMigrates(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Write legacy format (no schema_version)
	legacyContent := `active_profile: local
hub:
  endpoint: https://hub.example.com
  brokerId: old-broker
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(legacyContent), 0644))

	// UpdateSetting should auto-migrate legacy to v1 and apply the update
	err := UpdateSetting(projectDir, "grove_id", "my-grove-id", false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(projectDir, "settings.yaml"))
	require.NoError(t, err)

	// Should now be v1 format after auto-migration
	version, _ := DetectSettingsFormat(data)
	assert.Equal(t, "1", version, "legacy file should be migrated to v1 after UpdateSetting")

	// Verify the update was applied (struct tags now use project_id)
	assert.Contains(t, string(data), "project_id: my-grove-id")

	// Verify original values were preserved
	assert.Contains(t, string(data), "active_profile: local")
}

func TestLoadSingleFileVersioned_Basic(t *testing.T) {
	tmpDir := t.TempDir()

	v1Content := `schema_version: "1"
active_profile: local
hub:
  endpoint: https://hub.example.com
  grove_id: test-grove
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(v1Content), 0644))

	vs, err := LoadSingleFileVersioned(tmpDir)
	require.NoError(t, err)

	assert.Equal(t, "1", vs.SchemaVersion)
	assert.Equal(t, "local", vs.ActiveProfile)
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "https://hub.example.com", vs.Hub.Endpoint)
	assert.Equal(t, "test-grove", vs.Hub.ProjectID)
}

func TestLoadSingleFileVersioned_NoFile(t *testing.T) {
	tmpDir := t.TempDir()

	vs, err := LoadSingleFileVersioned(tmpDir)
	require.NoError(t, err)

	assert.Equal(t, "1", vs.SchemaVersion)
	assert.Empty(t, vs.ActiveProfile)
}

func TestUpdateSetting_V1GlobalScope(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	v1Content := `schema_version: "1"
active_profile: local
hub:
  endpoint: https://hub.example.com
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(v1Content), 0644))

	// Update global setting with v1 format
	err := UpdateSetting(globalDir, "hub.brokerId", "global-broker-id", true)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(data)
	assert.Equal(t, "1", version, "global v1 file should preserve format")

	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, "global-broker-id", vs.Server.Broker.BrokerID)
	assert.Equal(t, "https://hub.example.com", vs.Hub.Endpoint)
}

// --- Telemetry settings tests ---

func TestLoadVersionedSettings_TelemetryRoundTrip(t *testing.T) {
	// Verify that a full telemetry config YAML can be marshaled, validated, and unmarshaled.
	data := []byte(`
schema_version: "1"
telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "https://otel.example.com"
    protocol: grpc
    headers:
      Authorization: "Bearer test-key"
    tls:
      enabled: true
      insecure_skip_verify: false
      ca_file: "/etc/ssl/certs/custom-root.pem"
    batch:
      max_size: 256
      timeout: "10s"
  hub:
    enabled: true
    report_interval: "60s"
  local:
    enabled: false
    file: "/tmp/telemetry.jsonl"
    console: true
  filter:
    enabled: true
    respect_debug_mode: true
    events:
      include: []
      exclude:
        - "agent.user.prompt"
    attributes:
      redact:
        - "prompt"
        - "user.email"
      hash:
        - "session_id"
    sampling:
      default: 0.5
      rates:
        "agent.tool.call": 0.1
  resource:
    service.name: "scion-agent"
    deployment.env: "staging"
`)
	// Validate against schema
	validationErrors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, validationErrors, "full telemetry config should be valid, got: %v", validationErrors)

	// Unmarshal into struct
	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))

	require.NotNil(t, vs.Telemetry)
	require.NotNil(t, vs.Telemetry.Enabled)
	assert.True(t, *vs.Telemetry.Enabled)

	require.NotNil(t, vs.Telemetry.Cloud)
	require.NotNil(t, vs.Telemetry.Cloud.Enabled)
	assert.True(t, *vs.Telemetry.Cloud.Enabled)
	assert.Equal(t, "https://otel.example.com", vs.Telemetry.Cloud.Endpoint)
	assert.Equal(t, "grpc", vs.Telemetry.Cloud.Protocol)
	assert.Equal(t, "Bearer test-key", vs.Telemetry.Cloud.Headers["Authorization"])

	require.NotNil(t, vs.Telemetry.Cloud.TLS)
	require.NotNil(t, vs.Telemetry.Cloud.TLS.Enabled)
	assert.True(t, *vs.Telemetry.Cloud.TLS.Enabled)
	require.NotNil(t, vs.Telemetry.Cloud.TLS.InsecureSkipVerify)
	assert.False(t, *vs.Telemetry.Cloud.TLS.InsecureSkipVerify)
	assert.Equal(t, "/etc/ssl/certs/custom-root.pem", vs.Telemetry.Cloud.TLS.CAFile)

	require.NotNil(t, vs.Telemetry.Cloud.Batch)
	assert.Equal(t, 256, vs.Telemetry.Cloud.Batch.MaxSize)
	assert.Equal(t, "10s", vs.Telemetry.Cloud.Batch.Timeout)

	require.NotNil(t, vs.Telemetry.Hub)
	require.NotNil(t, vs.Telemetry.Hub.Enabled)
	assert.True(t, *vs.Telemetry.Hub.Enabled)
	assert.Equal(t, "60s", vs.Telemetry.Hub.ReportInterval)

	require.NotNil(t, vs.Telemetry.Local)
	require.NotNil(t, vs.Telemetry.Local.Enabled)
	assert.False(t, *vs.Telemetry.Local.Enabled)
	assert.Equal(t, "/tmp/telemetry.jsonl", vs.Telemetry.Local.File)
	require.NotNil(t, vs.Telemetry.Local.Console)
	assert.True(t, *vs.Telemetry.Local.Console)

	require.NotNil(t, vs.Telemetry.Filter)
	require.NotNil(t, vs.Telemetry.Filter.Enabled)
	assert.True(t, *vs.Telemetry.Filter.Enabled)
	require.NotNil(t, vs.Telemetry.Filter.RespectDebugMode)
	assert.True(t, *vs.Telemetry.Filter.RespectDebugMode)

	require.NotNil(t, vs.Telemetry.Filter.Events)
	assert.Equal(t, []string{"agent.user.prompt"}, vs.Telemetry.Filter.Events.Exclude)

	require.NotNil(t, vs.Telemetry.Filter.Attributes)
	assert.Equal(t, []string{"prompt", "user.email"}, vs.Telemetry.Filter.Attributes.Redact)
	assert.Equal(t, []string{"session_id"}, vs.Telemetry.Filter.Attributes.Hash)

	require.NotNil(t, vs.Telemetry.Filter.Sampling)
	require.NotNil(t, vs.Telemetry.Filter.Sampling.Default)
	assert.Equal(t, 0.5, *vs.Telemetry.Filter.Sampling.Default)
	assert.Equal(t, 0.1, vs.Telemetry.Filter.Sampling.Rates["agent.tool.call"])

	assert.Equal(t, "scion-agent", vs.Telemetry.Resource["service.name"])
	assert.Equal(t, "staging", vs.Telemetry.Resource["deployment.env"])
}

func TestValidateSettings_TelemetryInvalidProtocol(t *testing.T) {
	data := []byte(`
schema_version: "1"
telemetry:
  cloud:
    protocol: "invalid"
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "invalid protocol should produce validation error")
}

func TestValidateSettings_TelemetrySamplingOutOfRange(t *testing.T) {
	data := []byte(`
schema_version: "1"
telemetry:
  filter:
    sampling:
      default: 1.5
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "sampling rate > 1.0 should produce validation error")
}

func TestValidateSettings_TelemetryUnknownField(t *testing.T) {
	data := []byte(`
schema_version: "1"
telemetry:
  unknown_field: true
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "unknown field in telemetry should produce validation error")
}

func TestLoadVersionedSettings_TelemetryHierarchyMerge(t *testing.T) {
	// Unset all SCION_ environment variables to avoid pollution
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SCION_") {
			key := strings.SplitN(e, "=", 2)[0]
			val := os.Getenv(key)
			_ = os.Unsetenv(key)
			defer func() { _ = os.Setenv(key, val) }()
		}
	}

	// Test that telemetry settings merge across global → project (last write wins).
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()

	// Create global settings with telemetry defaults
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	globalSettings := `schema_version: "1"
telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "https://global-otel.example.com"
    protocol: grpc
  hub:
    enabled: true
    report_interval: "30s"
  filter:
    events:
      exclude:
        - "agent.user.prompt"
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	// Create project with overrides
	projectDir := filepath.Join(tmpDir, "myproject")
	projectScionDir := filepath.Join(projectDir, ".scion")
	require.NoError(t, os.MkdirAll(projectScionDir, 0755))
	_ = os.Chdir(projectDir)

	projectSettings := `schema_version: "1"
telemetry:
  cloud:
    endpoint: "https://grove-otel.example.com"
  hub:
    enabled: false
`
	require.NoError(t, os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(projectSettings), 0644))

	// Load merged settings
	vs, err := LoadVersionedSettings(projectScionDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Telemetry)

	// Telemetry.enabled should come from global (not overridden by project)
	require.NotNil(t, vs.Telemetry.Enabled)
	assert.True(t, *vs.Telemetry.Enabled)

	// Cloud endpoint should be overridden by project
	require.NotNil(t, vs.Telemetry.Cloud)
	assert.Equal(t, "https://grove-otel.example.com", vs.Telemetry.Cloud.Endpoint)

	// Cloud protocol should come from global
	assert.Equal(t, "grpc", vs.Telemetry.Cloud.Protocol)

	// Hub.enabled should be overridden by project
	require.NotNil(t, vs.Telemetry.Hub)
	require.NotNil(t, vs.Telemetry.Hub.Enabled)
	assert.False(t, *vs.Telemetry.Hub.Enabled)

	// Hub report_interval should come from global
	assert.Equal(t, "30s", vs.Telemetry.Hub.ReportInterval)
}

func TestVersionedEnvKeyMapper_Telemetry(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"SCION_TELEMETRY_ENABLED", "telemetry.enabled"},
		{"SCION_TELEMETRY_CLOUD_ENABLED", "telemetry.cloud.enabled"},
		{"SCION_TELEMETRY_CLOUD_ENDPOINT", "telemetry.cloud.endpoint"},
		{"SCION_TELEMETRY_CLOUD_PROTOCOL", "telemetry.cloud.protocol"},
		{"SCION_TELEMETRY_CLOUD_TLS_INSECURE_SKIP_VERIFY", "telemetry.cloud.tls.insecure_skip_verify"},
		{"SCION_TELEMETRY_CLOUD_BATCH_MAX_SIZE", "telemetry.cloud.batch.max_size"},
		{"SCION_TELEMETRY_CLOUD_BATCH_TIMEOUT", "telemetry.cloud.batch.timeout"},
		{"SCION_TELEMETRY_HUB_ENABLED", "telemetry.hub.enabled"},
		{"SCION_TELEMETRY_HUB_REPORT_INTERVAL", "telemetry.hub.report_interval"},
		{"SCION_TELEMETRY_LOCAL_ENABLED", "telemetry.local.enabled"},
		{"SCION_TELEMETRY_LOCAL_FILE", "telemetry.local.file"},
		{"SCION_TELEMETRY_LOCAL_CONSOLE", "telemetry.local.console"},
		{"SCION_TELEMETRY_FILTER_ENABLED", "telemetry.filter.enabled"},
		{"SCION_TELEMETRY_FILTER_RESPECT_DEBUG_MODE", "telemetry.filter.respect_debug_mode"},
		// OTEL env vars map to telemetry.cloud.*
		{"SCION_OTEL_ENDPOINT", "telemetry.cloud.endpoint"},
		{"SCION_OTEL_PROTOCOL", "telemetry.cloud.protocol"},
		{"SCION_OTEL_HEADERS", "telemetry.cloud.headers"},
		{"SCION_OTEL_INSECURE", "telemetry.cloud.tls.insecure_skip_verify"},
		{"SCION_OTEL_CA_FILE", "telemetry.cloud.tls.ca_file"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := versionedEnvKeyMapper(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLoadVersionedSettings_TelemetryEnvOverride(t *testing.T) {
	// Unset all SCION_ environment variables to avoid pollution
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SCION_") {
			key := strings.SplitN(e, "=", 2)[0]
			val := os.Getenv(key)
			_ = os.Unsetenv(key)
			defer func() { _ = os.Setenv(key, val) }()
		}
	}

	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()
	_ = os.Chdir(tmpDir)

	// Create global settings with telemetry
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	globalSettings := `schema_version: "1"
telemetry:
  enabled: true
  cloud:
    endpoint: "https://yaml-endpoint.example.com"
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	// Set env var override
	t.Setenv("SCION_TELEMETRY_ENABLED", "false")

	vs, err := LoadVersionedSettings("")
	require.NoError(t, err)
	require.NotNil(t, vs.Telemetry)

	// Env var should override the YAML setting
	require.NotNil(t, vs.Telemetry.Enabled)
	assert.False(t, *vs.Telemetry.Enabled)

	// YAML value should be preserved for non-overridden fields
	require.NotNil(t, vs.Telemetry.Cloud)
	assert.Equal(t, "https://yaml-endpoint.example.com", vs.Telemetry.Cloud.Endpoint)
}

// --- RewriteImageRegistry tests ---

func TestRewriteImageRegistry(t *testing.T) {
	tests := []struct {
		name        string
		fullImage   string
		newRegistry string
		want        string
	}{
		{
			name:        "fully qualified scion image preserved",
			fullImage:   "us-central1-docker.pkg.dev/example-project/scion-images/scion-claude:latest",
			newRegistry: "ghcr.io/myorg",
			want:        "us-central1-docker.pkg.dev/example-project/scion-images/scion-claude:latest",
		},
		{
			name:        "fully qualified scion image preserved with trailing slash",
			fullImage:   "us-central1-docker.pkg.dev/example-project/scion-images/scion-gemini:latest",
			newRegistry: "ghcr.io/myorg/",
			want:        "us-central1-docker.pkg.dev/example-project/scion-images/scion-gemini:latest",
		},
		{
			name:        "bare non-scion image rewritten",
			fullImage:   "ubuntu:22.04",
			newRegistry: "ghcr.io/myorg",
			want:        "ghcr.io/myorg/ubuntu:22.04",
		},
		{
			name:        "do not rewrite custom registry image",
			fullImage:   "myregistry.io/custom-agent:v1",
			newRegistry: "ghcr.io/myorg",
			want:        "myregistry.io/custom-agent:v1",
		},
		{
			name:        "bare custom harness image rewritten",
			fullImage:   "my-custom-harness:latest",
			newRegistry: "ghcr.io/myorg",
			want:        "ghcr.io/myorg/my-custom-harness:latest",
		},
		{
			name:        "empty registry returns original",
			fullImage:   "us-central1-docker.pkg.dev/example-project/scion-images/scion-claude:latest",
			newRegistry: "",
			want:        "us-central1-docker.pkg.dev/example-project/scion-images/scion-claude:latest",
		},
		{
			name:        "empty image returns empty",
			fullImage:   "",
			newRegistry: "ghcr.io/myorg",
			want:        "",
		},
		{
			name:        "fully qualified scion-base image preserved",
			fullImage:   "us-central1-docker.pkg.dev/example-project/scion-images/scion-base:v2",
			newRegistry: "docker.io/myuser",
			want:        "us-central1-docker.pkg.dev/example-project/scion-images/scion-base:v2",
		},
		{
			name:        "fully qualified preserves tag",
			fullImage:   "us-central1-docker.pkg.dev/example-project/scion-images/scion-opencode:sha-abc123",
			newRegistry: "ghcr.io/myorg",
			want:        "us-central1-docker.pkg.dev/example-project/scion-images/scion-opencode:sha-abc123",
		},
		{
			name:        "bare image name (no registry prefix)",
			fullImage:   "scion-claude:latest",
			newRegistry: "ghcr.io/myorg",
			want:        "ghcr.io/myorg/scion-claude:latest",
		},
		{
			name:        "bare image name empty registry",
			fullImage:   "scion-gemini:latest",
			newRegistry: "",
			want:        "scion-gemini:latest",
		},
		{
			name:        "explicit registry preserved",
			fullImage:   "ghcr.io/myorg/scion-elixir:latest",
			newRegistry: "docker.io/other",
			want:        "ghcr.io/myorg/scion-elixir:latest",
		},
		{
			name:        "port-based registry preserved",
			fullImage:   "localhost:5000/scion-test:latest",
			newRegistry: "ghcr.io/myorg",
			want:        "localhost:5000/scion-test:latest",
		},
		{
			name:        "docker hub path rewritten",
			fullImage:   "myuser/scion-custom:latest",
			newRegistry: "ghcr.io/myorg",
			want:        "ghcr.io/myorg/scion-custom:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RewriteImageRegistry(tt.fullImage, tt.newRegistry)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveImageRegistry(t *testing.T) {
	tests := []struct {
		name        string
		settings    *VersionedSettings
		profileName string
		want        string
	}{
		{
			name: "top-level image_registry",
			settings: &VersionedSettings{
				ActiveProfile: "local",
				ImageRegistry: "ghcr.io/myorg",
				Profiles: map[string]V1ProfileConfig{
					"local": {Runtime: "docker"},
				},
			},
			profileName: "local",
			want:        "ghcr.io/myorg",
		},
		{
			name: "profile-level overrides top-level",
			settings: &VersionedSettings{
				ActiveProfile: "staging",
				ImageRegistry: "ghcr.io/myorg",
				Profiles: map[string]V1ProfileConfig{
					"staging": {
						Runtime:       "docker",
						ImageRegistry: "us-central1-docker.pkg.dev/myproject/staging",
					},
				},
			},
			profileName: "staging",
			want:        "us-central1-docker.pkg.dev/myproject/staging",
		},
		{
			name: "profile without image_registry falls back to top-level",
			settings: &VersionedSettings{
				ActiveProfile: "local",
				ImageRegistry: "ghcr.io/myorg",
				Profiles: map[string]V1ProfileConfig{
					"local": {Runtime: "docker"},
				},
			},
			profileName: "local",
			want:        "ghcr.io/myorg",
		},
		{
			name: "empty profile name uses active profile",
			settings: &VersionedSettings{
				ActiveProfile: "prod",
				ImageRegistry: "ghcr.io/default",
				Profiles: map[string]V1ProfileConfig{
					"prod": {
						Runtime:       "docker",
						ImageRegistry: "ghcr.io/prod",
					},
				},
			},
			profileName: "",
			want:        "ghcr.io/prod",
		},
		{
			name: "no image_registry configured",
			settings: &VersionedSettings{
				ActiveProfile: "local",
				Profiles: map[string]V1ProfileConfig{
					"local": {Runtime: "docker"},
				},
			},
			profileName: "local",
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.settings.ResolveImageRegistry(tt.profileName)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestImageRegistryYAMLRoundTrip(t *testing.T) {
	vs := &VersionedSettings{
		SchemaVersion: "1",
		ActiveProfile: "local",
		ImageRegistry: "ghcr.io/myorg",
		Profiles: map[string]V1ProfileConfig{
			"local": {
				Runtime: "docker",
			},
			"staging": {
				Runtime:       "docker",
				ImageRegistry: "us-central1-docker.pkg.dev/myproject/staging",
			},
		},
		HarnessConfigs: map[string]HarnessConfigEntry{},
		Runtimes:       map[string]V1RuntimeConfig{},
	}

	data, err := yaml.Marshal(vs)
	require.NoError(t, err)

	assert.Contains(t, string(data), "image_registry: ghcr.io/myorg")
	assert.Contains(t, string(data), "image_registry: us-central1-docker.pkg.dev/myproject/staging")

	var roundTripped VersionedSettings
	err = yaml.Unmarshal(data, &roundTripped)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/myorg", roundTripped.ImageRegistry)
	assert.Equal(t, "us-central1-docker.pkg.dev/myproject/staging", roundTripped.Profiles["staging"].ImageRegistry)
	assert.Empty(t, roundTripped.Profiles["local"].ImageRegistry)
}

func TestUpdateVersionedSetting_ImageRegistry(t *testing.T) {
	dir := t.TempDir()
	initial := &VersionedSettings{
		SchemaVersion:  "1",
		ActiveProfile:  "local",
		Profiles:       map[string]V1ProfileConfig{},
		HarnessConfigs: map[string]HarnessConfigEntry{},
		Runtimes:       map[string]V1RuntimeConfig{},
	}
	err := SaveVersionedSettings(dir, initial)
	require.NoError(t, err)

	err = UpdateVersionedSetting(dir, "image_registry", "ghcr.io/myorg")
	require.NoError(t, err)

	loaded, err := LoadSingleFileVersioned(dir)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/myorg", loaded.ImageRegistry)
}

func TestUpdateVersionedSetting_DefaultHarnessConfig(t *testing.T) {
	dir := t.TempDir()
	initial := &VersionedSettings{
		SchemaVersion:  "1",
		ActiveProfile:  "local",
		Profiles:       map[string]V1ProfileConfig{},
		HarnessConfigs: map[string]HarnessConfigEntry{},
		Runtimes:       map[string]V1RuntimeConfig{},
	}
	err := SaveVersionedSettings(dir, initial)
	require.NoError(t, err)

	err = UpdateVersionedSetting(dir, "default_harness_config", "claude")
	require.NoError(t, err)

	loaded, err := LoadSingleFileVersioned(dir)
	require.NoError(t, err)
	assert.Equal(t, "claude", loaded.DefaultHarnessConfig)
}

func TestGetVersionedSettingValue(t *testing.T) {
	autohelp := true
	enabled := false
	linked := true
	localOnly := true

	vs := &VersionedSettings{
		SchemaVersion:        "1",
		ActiveProfile:        "staging",
		DefaultTemplate:      "my-template",
		DefaultHarnessConfig: "claude",
		ImageRegistry:        "ghcr.io/myorg",
		CLI:                  &V1CLIConfig{AutoHelp: &autohelp},
		Hub: &V1HubClientConfig{
			Enabled:   &enabled,
			Linked:    &linked,
			Endpoint:  "https://hub.example.com",
			ProjectID: "grove-123",
			LocalOnly: &localOnly,
		},
		Server: &V1ServerConfig{
			Broker: &V1BrokerConfig{
				BrokerID:       "broker-1",
				BrokerToken:    "tok-secret",
				BrokerNickname: "my-broker",
			},
		},
	}

	tests := []struct {
		key  string
		want string
	}{
		{"active_profile", "staging"},
		{"default_template", "my-template"},
		{"default_harness_config", "claude"},
		{"image_registry", "ghcr.io/myorg"},
		{"cli.autohelp", "true"},
		{"grove_id", "grove-123"},
		{"hub.enabled", "false"},
		{"hub.linked", "true"},
		{"hub.endpoint", "https://hub.example.com"},
		{"hub.groveId", "grove-123"},
		{"hub.local_only", "true"},
		{"hub.brokerId", "broker-1"},
		{"hub.brokerToken", "tok-secret"},
		{"hub.brokerNickname", "my-broker"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, err := GetVersionedSettingValue(vs, tt.key)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	// Unknown key should error
	_, err := GetVersionedSettingValue(vs, "nonexistent_key")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown or complex setting key")

	// Nil sub-structs should return empty strings
	empty := &VersionedSettings{SchemaVersion: "1"}
	for _, key := range []string{"grove_id", "hub.endpoint", "hub.brokerId", "cli.autohelp"} {
		got, err := GetVersionedSettingValue(empty, key)
		require.NoError(t, err, "key=%s", key)
		assert.Empty(t, got, "key=%s", key)
	}
}

func TestIsImageRegistryConfigured(t *testing.T) {
	tests := []struct {
		name     string
		settings *VersionedSettings
		profile  string
		want     bool
	}{
		{
			name:     "empty settings",
			settings: &VersionedSettings{},
			profile:  "",
			want:     false,
		},
		{
			name: "top-level registry set",
			settings: &VersionedSettings{
				ImageRegistry: "ghcr.io/myorg",
			},
			profile: "",
			want:    true,
		},
		{
			name: "profile-level registry set",
			settings: &VersionedSettings{
				ActiveProfile: "local",
				Profiles: map[string]V1ProfileConfig{
					"local": {ImageRegistry: "ghcr.io/myorg"},
				},
			},
			profile: "local",
			want:    true,
		},
		{
			name: "no registry anywhere",
			settings: &VersionedSettings{
				ActiveProfile: "local",
				Profiles: map[string]V1ProfileConfig{
					"local": {Runtime: "docker"},
				},
			},
			profile: "local",
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.settings.IsImageRegistryConfigured(tt.profile)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRequireImageRegistry_NotConfigured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	// Create a minimal versioned settings file without image_registry
	vs := &VersionedSettings{
		SchemaVersion: "1",
		ActiveProfile: "local",
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
		HarnessConfigs: map[string]HarnessConfigEntry{},
		Runtimes:       map[string]V1RuntimeConfig{},
	}
	require.NoError(t, SaveVersionedSettings(dir, vs))

	err := RequireImageRegistry(dir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image_registry is not configured")
	assert.Contains(t, err.Error(), "image-build/README.md")
}

func TestRequireImageRegistry_Configured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	vs := &VersionedSettings{
		SchemaVersion: "1",
		ActiveProfile: "local",
		ImageRegistry: "ghcr.io/myorg",
		Profiles: map[string]V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
		HarnessConfigs: map[string]HarnessConfigEntry{},
		Runtimes:       map[string]V1RuntimeConfig{},
	}
	require.NoError(t, SaveVersionedSettings(dir, vs))

	err := RequireImageRegistry(dir, "")
	assert.NoError(t, err)
}

// --- N0-1: Workspace storage config tests ---

func TestWorkspaceStorageConfig_YAMLRoundTrip(t *testing.T) {
	yamlInput := `
schema_version: "1"
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: /mnt/nfs
      mount_options: "vers=4.1,hard,nconnect=8"
      uid: 2000
      gid: 2000
      subpath_root: workspaces
      storage_class: filestore-sc
      shares:
        - id: share-1
          server: "10.0.0.2"
          export: /scion-workspaces
          pv_name: scion-workspaces-pv
`
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(yamlInput), 0644))

	projectDir := filepath.Join(tmpDir, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.WorkspaceStorage)

	ws := vs.Server.WorkspaceStorage
	assert.Equal(t, "nfs", ws.Backend)
	require.NotNil(t, ws.NFS)
	assert.Equal(t, "/mnt/nfs", ws.NFS.MountRoot)
	assert.Equal(t, "vers=4.1,hard,nconnect=8", ws.NFS.MountOptions)
	assert.Equal(t, 2000, ws.NFS.UID)
	assert.Equal(t, 2000, ws.NFS.GID)
	assert.Equal(t, "workspaces", ws.NFS.SubPathRoot)
	assert.Equal(t, "filestore-sc", ws.NFS.StorageClass)
	require.Len(t, ws.NFS.Shares, 1)
	assert.Equal(t, "share-1", ws.NFS.Shares[0].ID)
	assert.Equal(t, "10.0.0.2", ws.NFS.Shares[0].Server)
	assert.Equal(t, "/scion-workspaces", ws.NFS.Shares[0].Export)
	assert.Equal(t, "scion-workspaces-pv", ws.NFS.Shares[0].PVName)
}

func TestWorkspaceStorageConfig_JSONRoundTrip(t *testing.T) {
	ws := &V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &V1NFSConfig{
			MountRoot:    "/mnt/nfs",
			MountOptions: "vers=4.1,hard,nconnect=4,_netdev",
			UID:          1000,
			GID:          1000,
			SubPathRoot:  "projects",
			Shares: []V1NFSShare{
				{ID: "main", Server: "10.0.0.2", Export: "/scion-workspaces", PVName: "scion-ws-pv"},
			},
		},
	}

	data, err := json.Marshal(ws)
	require.NoError(t, err)

	var roundTripped V1WorkspaceStorageConfig
	require.NoError(t, json.Unmarshal(data, &roundTripped))

	assert.Equal(t, ws.Backend, roundTripped.Backend)
	require.NotNil(t, roundTripped.NFS)
	assert.Equal(t, ws.NFS.MountRoot, roundTripped.NFS.MountRoot)
	assert.Equal(t, ws.NFS.MountOptions, roundTripped.NFS.MountOptions)
	assert.Equal(t, ws.NFS.UID, roundTripped.NFS.UID)
	assert.Equal(t, ws.NFS.GID, roundTripped.NFS.GID)
	assert.Equal(t, ws.NFS.SubPathRoot, roundTripped.NFS.SubPathRoot)
	require.Len(t, roundTripped.NFS.Shares, 1)
	assert.Equal(t, ws.NFS.Shares[0].ID, roundTripped.NFS.Shares[0].ID)
	assert.Equal(t, ws.NFS.Shares[0].Server, roundTripped.NFS.Shares[0].Server)
}

func TestWorkspaceStorageConfig_NFSDefaults(t *testing.T) {
	t.Run("nfs backend applies defaults to empty sub-fields", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "nfs"}
		ws.ApplyNFSDefaults()

		require.NotNil(t, ws.NFS)
		assert.Equal(t, "vers=3,hard,nconnect=4,_netdev", ws.NFS.MountOptions)
		assert.Equal(t, 1000, ws.NFS.UID)
		assert.Equal(t, 1000, ws.NFS.GID)
		assert.Equal(t, "projects", ws.NFS.SubPathRoot)
	})

	t.Run("nfs backend preserves explicit values", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				MountOptions: "custom-opts",
				UID:          5000,
				GID:          5000,
				SubPathRoot:  "custom-root",
			},
		}
		ws.ApplyNFSDefaults()

		assert.Equal(t, "custom-opts", ws.NFS.MountOptions)
		assert.Equal(t, 5000, ws.NFS.UID)
		assert.Equal(t, 5000, ws.NFS.GID)
		assert.Equal(t, "custom-root", ws.NFS.SubPathRoot)
	})

	t.Run("local backend does not materialize NFS block", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "local"}
		ws.ApplyNFSDefaults()
		assert.Nil(t, ws.NFS)
	})

	t.Run("empty backend does not materialize NFS block", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{}
		ws.ApplyNFSDefaults()
		assert.Nil(t, ws.NFS)
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var ws *V1WorkspaceStorageConfig
		ws.ApplyNFSDefaults() // should not panic
	})
}

func TestWorkspaceStorageConfig_ValidateNFS(t *testing.T) {
	t.Run("nfs backend with no shares returns error", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "nfs"}
		ws.ApplyNFSDefaults()
		err := ws.ValidateNFS()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no NFS shares are defined")
	})

	t.Run("nfs backend with shares passes", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{
			Backend: "nfs",
			NFS: &V1NFSConfig{
				Shares: []V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/data"}},
			},
		}
		ws.ApplyNFSDefaults()
		err := ws.ValidateNFS()
		require.NoError(t, err)
	})

	t.Run("local backend skips validation", func(t *testing.T) {
		ws := &V1WorkspaceStorageConfig{Backend: "local"}
		err := ws.ValidateNFS()
		require.NoError(t, err)
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var ws *V1WorkspaceStorageConfig
		err := ws.ValidateNFS()
		require.NoError(t, err)
	})
}

func TestWorkspaceStorageConfig_BackendUnset_IsLocal(t *testing.T) {
	// Backend unset => treated as "local", no NFS struct required.
	ws := &V1WorkspaceStorageConfig{}
	assert.Equal(t, "", ws.Backend, "empty backend is treated as local")
	assert.Nil(t, ws.NFS, "no NFS block when backend is local/empty")
}

// ============================================================================
// Scheduler Config Tests
// ============================================================================

func TestConvertV1ServerToGlobalConfig_Scheduler(t *testing.T) {
	v1 := &V1ServerConfig{
		Scheduler: &V1SchedulerConfig{
			IntervalSeconds: 120,
			MaxConcurrency:  intPtr(3),
		},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, 120, gc.Scheduler.IntervalSeconds)
	require.NotNil(t, gc.Scheduler.MaxConcurrency)
	assert.Equal(t, 3, *gc.Scheduler.MaxConcurrency)
}

func TestConvertV1ServerToGlobalConfig_SchedulerNil(t *testing.T) {
	v1 := &V1ServerConfig{}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, 0, gc.Scheduler.IntervalSeconds, "nil scheduler should leave defaults (zero)")
	assert.Nil(t, gc.Scheduler.MaxConcurrency, "nil scheduler should leave MaxConcurrency nil (use scheduler default)")
}

func TestConvertGlobalToV1ServerConfig_Scheduler(t *testing.T) {
	gc := DefaultGlobalConfig()
	gc.Scheduler.IntervalSeconds = 180
	gc.Scheduler.MaxConcurrency = intPtr(2)
	v1 := ConvertGlobalToV1ServerConfig(&gc)
	require.NotNil(t, v1.Scheduler)
	assert.Equal(t, 180, v1.Scheduler.IntervalSeconds)
	require.NotNil(t, v1.Scheduler.MaxConcurrency)
	assert.Equal(t, 2, *v1.Scheduler.MaxConcurrency)
}

func TestConvertGlobalToV1ServerConfig_SchedulerZeroOmitted(t *testing.T) {
	gc := DefaultGlobalConfig()
	// Zero/nil values — scheduler block should not be emitted
	v1 := ConvertGlobalToV1ServerConfig(&gc)
	assert.Nil(t, v1.Scheduler, "zero-value scheduler should be omitted in V1")
}

func TestSchedulerMaxConcurrency_ExplicitZeroRoundTrips(t *testing.T) {
	// Regression: explicit max_concurrency=0 (unlimited) must survive the
	// V1 → Global → V1 round-trip and not be confused with "unset".
	v1 := &V1ServerConfig{
		Scheduler: &V1SchedulerConfig{
			MaxConcurrency: intPtr(0),
		},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	require.NotNil(t, gc.Scheduler.MaxConcurrency, "explicit 0 must not be nil")
	assert.Equal(t, 0, *gc.Scheduler.MaxConcurrency)

	v1Out := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1Out.Scheduler, "scheduler block must be emitted for explicit 0")
	require.NotNil(t, v1Out.Scheduler.MaxConcurrency)
	assert.Equal(t, 0, *v1Out.Scheduler.MaxConcurrency)
}

func TestVersionedEnvKeyMapper_Scheduler(t *testing.T) {
	tests := []struct {
		env  string
		want string
	}{
		{"SCION_SERVER_SCHEDULER_INTERVAL_SECONDS", "server.scheduler.interval_seconds"},
		{"SCION_SERVER_SCHEDULER_MAX_CONCURRENCY", "server.scheduler.max_concurrency"},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			got := versionedEnvKeyMapper(tt.env)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestConvertV1ServerToGlobalConfig_StalledThresholdValid(t *testing.T) {
	v1 := &V1ServerConfig{
		Hub: &V1ServerHubConfig{
			StalledThreshold: "10m",
		},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, 10*time.Minute, gc.Hub.StalledThreshold)
}

func TestConvertV1ServerToGlobalConfig_StalledThresholdInvalidIgnored(t *testing.T) {
	v1 := &V1ServerConfig{
		Hub: &V1ServerHubConfig{
			StalledThreshold: "not-a-duration",
		},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	// Invalid duration string should be ignored; StalledThreshold stays at zero value.
	assert.Equal(t, time.Duration(0), gc.Hub.StalledThreshold)
}

// --- Federation conversion tests ---

func TestConvertV1FederationConfig_RoundTrip(t *testing.T) {
	enabled := true
	v1 := &V1ServerConfig{
		Federation: &V1FederationConfig{
			Enabled: &enabled,
			TrustedIssuers: []V1TrustedIssuerConfig{
				{
					IssuerURL:        "https://hub-a.example.com",
					JWKSURL:          "https://hub-a.example.com/.well-known/jwks.json",
					ExpectedAudience: "https://hub-b.example.com",
					AllowedProjects:  []string{"proj1", "proj2"},
					AllowedRootUsers: []string{"user@example.com"},
					DefaultScopes:    []string{"agent:status:update", "agent:message:send"},
					IssuerType:       "hub",
					DefaultRole:      "",
					AllowedEmails:    nil,
				},
				{
					IssuerURL:        "https://accounts.google.com",
					JWKSURL:          "",
					ExpectedAudience: "https://hub-b.example.com",
					AllowedProjects:  nil,
					AllowedRootUsers: nil,
					DefaultScopes:    []string{"agent:status:update"},
					IssuerType:       "service_account",
					DefaultRole:      "",
					AllowedEmails:    []string{"sa@proj.iam.gserviceaccount.com"},
				},
			},
			Algorithms:       []string{"RS256", "ES256"},
			RefreshInterval:  "1h",
			DebounceInterval: "5s",
		},
	}

	// V1 -> GlobalConfig
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.True(t, gc.Federation.Enabled)
	require.Len(t, gc.Federation.TrustedIssuers, 2)

	ti0 := gc.Federation.TrustedIssuers[0]
	assert.Equal(t, "https://hub-a.example.com", ti0.IssuerURL)
	assert.Equal(t, "https://hub-a.example.com/.well-known/jwks.json", ti0.JWKSURL)
	assert.Equal(t, "https://hub-b.example.com", ti0.ExpectedAudience)
	assert.Equal(t, []string{"proj1", "proj2"}, ti0.AllowedProjects)
	assert.Equal(t, []string{"user@example.com"}, ti0.AllowedRootUsers)
	assert.Equal(t, []string{"agent:status:update", "agent:message:send"}, ti0.DefaultScopes)
	assert.Equal(t, "hub", ti0.IssuerType)

	ti1 := gc.Federation.TrustedIssuers[1]
	assert.Equal(t, "https://accounts.google.com", ti1.IssuerURL)
	assert.Equal(t, "service_account", ti1.IssuerType)
	assert.Equal(t, []string{"sa@proj.iam.gserviceaccount.com"}, ti1.AllowedEmails)

	assert.Equal(t, []string{"RS256", "ES256"}, gc.Federation.Algorithms)
	assert.Equal(t, time.Hour, gc.Federation.Cache.RefreshInterval)
	assert.Equal(t, 5*time.Second, gc.Federation.Cache.DebounceInterval)

	// GlobalConfig -> V1 (round-trip back)
	v1Back := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1Back.Federation)
	assert.Equal(t, &enabled, v1Back.Federation.Enabled)
	assert.Equal(t, []string{"RS256", "ES256"}, v1Back.Federation.Algorithms)
	assert.Equal(t, "1h0m0s", v1Back.Federation.RefreshInterval)
	assert.Equal(t, "5s", v1Back.Federation.DebounceInterval)

	require.Len(t, v1Back.Federation.TrustedIssuers, 2)
	vi0 := v1Back.Federation.TrustedIssuers[0]
	assert.Equal(t, "https://hub-a.example.com", vi0.IssuerURL)
	assert.Equal(t, "https://hub-a.example.com/.well-known/jwks.json", vi0.JWKSURL)
	assert.Equal(t, "https://hub-b.example.com", vi0.ExpectedAudience)
	assert.Equal(t, []string{"proj1", "proj2"}, vi0.AllowedProjects)
	assert.Equal(t, []string{"user@example.com"}, vi0.AllowedRootUsers)
	assert.Equal(t, []string{"agent:status:update", "agent:message:send"}, vi0.DefaultScopes)
	assert.Equal(t, "hub", vi0.IssuerType)

	vi1 := v1Back.Federation.TrustedIssuers[1]
	assert.Equal(t, "https://accounts.google.com", vi1.IssuerURL)
	assert.Equal(t, "service_account", vi1.IssuerType)
	assert.Equal(t, []string{"sa@proj.iam.gserviceaccount.com"}, vi1.AllowedEmails)
}

func TestConvertV1FederationConfig_NilFederation(t *testing.T) {
	v1 := &V1ServerConfig{}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.False(t, gc.Federation.Enabled)
	assert.Empty(t, gc.Federation.TrustedIssuers)
}

func TestConvertGlobalToV1_FederationDisabledNoIssuers(t *testing.T) {
	gc := DefaultGlobalConfig()
	gc.Federation.Enabled = false
	gc.Federation.TrustedIssuers = nil
	v1 := ConvertGlobalToV1ServerConfig(&gc)
	// When federation is disabled with no issuers, Federation should be nil in V1
	assert.Nil(t, v1.Federation)
}

func TestConvertV1FederationConfig_EnabledNilDereference(t *testing.T) {
	// Test that nil Enabled is handled safely (defaults to false)
	v1 := &V1ServerConfig{
		Federation: &V1FederationConfig{
			Enabled: nil,
			TrustedIssuers: []V1TrustedIssuerConfig{
				{
					IssuerURL: "https://hub-a.example.com",
				},
			},
		},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.False(t, gc.Federation.Enabled)
	require.Len(t, gc.Federation.TrustedIssuers, 1)
	assert.Equal(t, "https://hub-a.example.com", gc.Federation.TrustedIssuers[0].IssuerURL)
}

// --- OIDC Login Config Tests ---

func TestConvertV1ServerToGlobalConfig_OIDCLogin(t *testing.T) {
	enabled := true
	v1 := &V1ServerConfig{
		OIDCLogin: &V1OIDCLoginConfig{
			Enabled:      &enabled,
			DisplayName:  "Corporate SSO",
			IssuerURL:    "https://sso.example.com/auth/realms/main",
			ClientID:     "scion-client",
			ClientSecret: "secret-value",
			Scopes:       []string{"openid", "email", "custom-scope"},
		},
	}

	gc := ConvertV1ServerToGlobalConfig(v1)

	assert.True(t, gc.OIDCLogin.Enabled)
	assert.Equal(t, "Corporate SSO", gc.OIDCLogin.DisplayName)
	assert.Equal(t, "https://sso.example.com/auth/realms/main", gc.OIDCLogin.IssuerURL)
	assert.Equal(t, "scion-client", gc.OIDCLogin.ClientID)
	assert.Equal(t, "secret-value", gc.OIDCLogin.ClientSecret)
	assert.Equal(t, []string{"openid", "email", "custom-scope"}, gc.OIDCLogin.Scopes)
}

func TestConvertV1ServerToGlobalConfig_OIDCLoginNil(t *testing.T) {
	v1 := &V1ServerConfig{}
	gc := ConvertV1ServerToGlobalConfig(v1)

	assert.False(t, gc.OIDCLogin.Enabled)
	assert.Empty(t, gc.OIDCLogin.DisplayName)
	assert.Empty(t, gc.OIDCLogin.IssuerURL)
	assert.Empty(t, gc.OIDCLogin.ClientID)
	assert.Empty(t, gc.OIDCLogin.ClientSecret)
	assert.Nil(t, gc.OIDCLogin.Scopes)
}

func TestConvertV1ServerToGlobalConfig_OIDCLoginDefaultScopes(t *testing.T) {
	enabled := true
	v1 := &V1ServerConfig{
		OIDCLogin: &V1OIDCLoginConfig{
			Enabled:   &enabled,
			IssuerURL: "https://sso.example.com",
			ClientID:  "client-id",
			// Scopes not set — should remain nil (defaults applied at usage time)
		},
	}

	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.True(t, gc.OIDCLogin.Enabled)
	assert.Nil(t, gc.OIDCLogin.Scopes)
}

func TestConvertGlobalToV1ServerConfig_OIDCLogin(t *testing.T) {
	gc := &GlobalConfig{
		OIDCLogin: OIDCLoginConfig{
			Enabled:      true,
			DisplayName:  "JB Hunt SSO",
			IssuerURL:    "https://sso.example.com/auth/realms/security360",
			ClientID:     "scion-test",
			ClientSecret: "test-secret",
			Scopes:       []string{"openid", "email"},
		},
	}

	v1 := ConvertGlobalToV1ServerConfig(gc)

	require.NotNil(t, v1.OIDCLogin)
	assert.Equal(t, boolPtr(true), v1.OIDCLogin.Enabled)
	assert.Equal(t, "JB Hunt SSO", v1.OIDCLogin.DisplayName)
	assert.Equal(t, "https://sso.example.com/auth/realms/security360", v1.OIDCLogin.IssuerURL)
	assert.Equal(t, "scion-test", v1.OIDCLogin.ClientID)
	assert.Equal(t, "test-secret", v1.OIDCLogin.ClientSecret)
	assert.Equal(t, []string{"openid", "email"}, v1.OIDCLogin.Scopes)
}

func TestConvertGlobalToV1ServerConfig_OIDCLoginDisabledOmitted(t *testing.T) {
	gc := &GlobalConfig{
		OIDCLogin: OIDCLoginConfig{
			Enabled: false,
			// IssuerURL is empty too, so it should be omitted
		},
	}

	v1 := ConvertGlobalToV1ServerConfig(gc)
	assert.Nil(t, v1.OIDCLogin)
}

func TestConvertV1OIDCLoginConfig_RoundTrip(t *testing.T) {
	original := &GlobalConfig{
		OIDCLogin: OIDCLoginConfig{
			Enabled:      true,
			DisplayName:  "Test SSO",
			IssuerURL:    "https://idp.example.com",
			ClientID:     "client-123",
			ClientSecret: "secret-456",
			Scopes:       []string{"openid", "email", "profile"},
		},
	}

	v1 := ConvertGlobalToV1ServerConfig(original)
	roundTripped := ConvertV1ServerToGlobalConfig(v1)

	assert.Equal(t, original.OIDCLogin.Enabled, roundTripped.OIDCLogin.Enabled)
	assert.Equal(t, original.OIDCLogin.DisplayName, roundTripped.OIDCLogin.DisplayName)
	assert.Equal(t, original.OIDCLogin.IssuerURL, roundTripped.OIDCLogin.IssuerURL)
	assert.Equal(t, original.OIDCLogin.ClientID, roundTripped.OIDCLogin.ClientID)
	assert.Equal(t, original.OIDCLogin.ClientSecret, roundTripped.OIDCLogin.ClientSecret)
	assert.Equal(t, original.OIDCLogin.Scopes, roundTripped.OIDCLogin.Scopes)
}

// --- Helper ---

func boolPtr(b bool) *bool {
	return &b
}

func intPtr(i int) *int {
	return &i
}
