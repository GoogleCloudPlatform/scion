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

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// --- Struct round-trip tests ---

func TestVersionedSettings_YAMLRoundTrip(t *testing.T) {
	autoHelp := true
	tmux := true

	vs := &VersionedSettings{
		SchemaVersion:   "1",
		ActiveProfile:   "local",
		DefaultTemplate: "gemini",
		Hub: &V1HubClientConfig{
			Enabled:  boolPtr(true),
			Endpoint: "https://hub.example.com",
			GroveID:  "test-grove",
		},
		CLI: &V1CLIConfig{
			AutoHelp:            &autoHelp,
			InteractiveDisabled: boolPtr(false),
		},
		Runtimes: map[string]V1RuntimeConfig{
			"docker": {Type: "docker", Host: ""},
			"container": {Type: "container", Tmux: &tmux},
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
				Tmux:                 &tmux,
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
	assert.Equal(t, vs.Hub.GroveID, roundTripped.Hub.GroveID)
	assert.Equal(t, vs.HarnessConfigs["gemini"].Model, roundTripped.HarnessConfigs["gemini"].Model)
	assert.Equal(t, vs.HarnessConfigs["gemini"].Args, roundTripped.HarnessConfigs["gemini"].Args)
	assert.Equal(t, vs.Profiles["local"].DefaultHarnessConfig, roundTripped.Profiles["local"].DefaultHarnessConfig)
}

// --- LoadVersionedSettings tests ---

func TestLoadVersionedSettings_DefaultsOnly(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	vs, err := LoadVersionedSettings(groveDir)
	require.NoError(t, err)

	assert.Equal(t, "1", vs.SchemaVersion)
	assert.Equal(t, "local", vs.ActiveProfile)
	assert.Equal(t, "default", vs.DefaultTemplate)
	assert.Equal(t, "gemini", vs.DefaultHarnessConfig)
	// harness_configs block is no longer in default settings (lives on disk as harness-config dirs)
	assert.Contains(t, vs.Runtimes, "docker")
	assert.Equal(t, "docker", vs.Runtimes["docker"].Type)
}

func TestLoadVersionedSettings_GlobalOverride(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	globalSettings := `
schema_version: "1"
active_profile: prod
default_template: claude
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	vs, err := LoadVersionedSettings(groveDir)
	require.NoError(t, err)

	assert.Equal(t, "prod", vs.ActiveProfile)
	assert.Equal(t, "claude", vs.DefaultTemplate)
}

func TestLoadVersionedSettings_GroveOverride(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	globalSettings := `
schema_version: "1"
active_profile: prod
default_template: claude
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	groveSettings := `
schema_version: "1"
active_profile: staging
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(groveSettings), 0644))

	vs, err := LoadVersionedSettings(groveDir)
	require.NoError(t, err)

	assert.Equal(t, "staging", vs.ActiveProfile)
	// Template should still be claude from global
	assert.Equal(t, "claude", vs.DefaultTemplate)
}

func TestLoadVersionedSettings_EnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	// Set environment variable overrides
	os.Setenv("SCION_ACTIVE_PROFILE", "remote")
	defer os.Unsetenv("SCION_ACTIVE_PROFILE")

	os.Setenv("SCION_DEFAULT_TEMPLATE", "opencode")
	defer os.Unsetenv("SCION_DEFAULT_TEMPLATE")

	vs, err := LoadVersionedSettings(groveDir)
	require.NoError(t, err)

	assert.Equal(t, "remote", vs.ActiveProfile)
	assert.Equal(t, "opencode", vs.DefaultTemplate)
}

func TestLoadVersionedSettings_HubEnvVars(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	// Test SCION_HUB_GROVE_ID maps correctly (regression test)
	os.Setenv("SCION_HUB_GROVE_ID", "my-grove-id")
	defer os.Unsetenv("SCION_HUB_GROVE_ID")

	os.Setenv("SCION_HUB_LOCAL_ONLY", "true")
	defer os.Unsetenv("SCION_HUB_LOCAL_ONLY")

	vs, err := LoadVersionedSettings(groveDir)
	require.NoError(t, err)

	require.NotNil(t, vs.Hub)
	assert.Equal(t, "my-grove-id", vs.Hub.GroveID)
}

func TestLoadVersionedSettings_CLIEnvVars(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	os.Setenv("SCION_CLI_AUTOHELP", "false")
	defer os.Unsetenv("SCION_CLI_AUTOHELP")

	os.Setenv("SCION_CLI_INTERACTIVE_DISABLED", "true")
	defer os.Unsetenv("SCION_CLI_INTERACTIVE_DISABLED")

	vs, err := LoadVersionedSettings(groveDir)
	require.NoError(t, err)

	require.NotNil(t, vs.CLI)
}

func TestLoadVersionedSettings_JSONFallback(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	// Write JSON settings (should load via JSON fallback)
	globalJSON := `{
		"schema_version": "1",
		"active_profile": "json-profile",
		"default_template": "json-template"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.json"), []byte(globalJSON), 0644))

	vs, err := LoadVersionedSettings(groveDir)
	require.NoError(t, err)

	assert.Equal(t, "json-profile", vs.ActiveProfile)
	assert.Equal(t, "json-template", vs.DefaultTemplate)
}

func TestLoadVersionedSettings_NewFields(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	groveSettings := `
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
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(groveSettings), 0644))

	vs, err := LoadVersionedSettings(groveDir)
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
	tmux := true

	legacy := &Settings{
		ActiveProfile:   "local",
		DefaultTemplate: "gemini",
		Hub: &HubClientConfig{
			Enabled:  &enabled,
			Endpoint: "https://hub.example.com",
			GroveID:  "test-grove",
		},
		CLI: &CLIConfig{
			AutoHelp: &autoHelp,
		},
		Runtimes: map[string]RuntimeConfig{
			"docker":    {Host: "tcp://localhost:2375"},
			"container": {Tmux: &tmux},
		},
		Harnesses: map[string]HarnessConfig{
			"gemini": {Image: "example.com/gemini:latest", User: "scion"},
			"claude": {Image: "example.com/claude:latest", User: "scion"},
		},
		Profiles: map[string]ProfileConfig{
			"local": {Runtime: "container", Tmux: &tmux},
		},
	}

	vs, warnings := AdaptLegacySettings(legacy)

	assert.Equal(t, "1", vs.SchemaVersion)
	assert.Equal(t, "local", vs.ActiveProfile)
	assert.Equal(t, "gemini", vs.DefaultTemplate)

	// Hub mapping
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "https://hub.example.com", vs.Hub.Endpoint)
	assert.Equal(t, "test-grove", vs.Hub.GroveID)
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
	tmux := true
	vs := &VersionedSettings{
		SchemaVersion:   "1",
		ActiveProfile:   "local",
		DefaultTemplate: "gemini",
		Hub: &V1HubClientConfig{
			Enabled:  boolPtr(true),
			Endpoint: "https://hub.example.com",
			GroveID:  "test-grove",
		},
		CLI: &V1CLIConfig{
			AutoHelp:            boolPtr(true),
			InteractiveDisabled: boolPtr(false),
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
				Tmux:                 &tmux,
			},
		},
	}

	legacy := convertVersionedToLegacy(vs)

	assert.Equal(t, "local", legacy.ActiveProfile)
	assert.Equal(t, "gemini", legacy.DefaultTemplate)

	// Hub — only v1 fields should be mapped
	require.NotNil(t, legacy.Hub)
	assert.Equal(t, "https://hub.example.com", legacy.Hub.Endpoint)
	assert.Equal(t, "test-grove", legacy.Hub.GroveID)
	assert.True(t, *legacy.Hub.Enabled)
	assert.Empty(t, legacy.Hub.Token) // Not in v1

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

func TestConvertVersionedToLegacy_Nil(t *testing.T) {
	legacy := convertVersionedToLegacy(nil)
	assert.NotNil(t, legacy)
	assert.Empty(t, legacy.ActiveProfile)
}

// --- LoadEffectiveSettings tests ---

func TestLoadEffectiveSettings_VersionedFileRouting(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	// Write versioned grove settings
	groveSettings := `
schema_version: "1"
active_profile: versioned-profile
harness_configs:
  gemini:
    harness: gemini
    image: example.com/gemini:latest
    user: scion
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(groveSettings), 0644))

	vs, warnings, err := LoadEffectiveSettings(groveDir)
	require.NoError(t, err)

	assert.Equal(t, "versioned-profile", vs.ActiveProfile)
	assert.Empty(t, warnings, "versioned path should produce no deprecation warnings")
}

func TestLoadEffectiveSettings_LegacyFileRouting(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	// Write legacy grove settings (has harnesses, no schema_version)
	groveSettings := `
active_profile: legacy-profile
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
profiles:
  legacy-profile:
    runtime: docker
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(groveSettings), 0644))

	vs, warnings, err := LoadEffectiveSettings(groveDir)
	require.NoError(t, err)

	assert.Equal(t, "legacy-profile", vs.ActiveProfile)
	assert.Equal(t, "1", vs.SchemaVersion) // Should be set by adapter
	assert.NotEmpty(t, warnings, "legacy path should produce deprecation warnings")
}

func TestLoadEffectiveSettings_NoUserFiles(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	// No settings files — should use defaults via legacy path
	vs, warnings, err := LoadEffectiveSettings(groveDir)
	require.NoError(t, err)

	assert.Equal(t, "local", vs.ActiveProfile)
	assert.Equal(t, "default", vs.DefaultTemplate)
	// Defaults flow through legacy path since no user files, so we get harness warnings
	// from the adaptation of embedded defaults
	_ = warnings
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
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	// Load via legacy path
	legacySettings, err := LoadSettingsKoanf(groveDir)
	require.NoError(t, err)
	adapted, _ := AdaptLegacySettings(legacySettings)

	// Load via versioned path
	versioned, err := LoadVersionedSettings(groveDir)
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

// --- resolveEffectiveGrovePath tests ---

func TestResolveEffectiveGrovePath_Global(t *testing.T) {
	result := resolveEffectiveGrovePath("global")
	assert.Equal(t, "", result, "global should resolve to empty (already loaded)")

	result = resolveEffectiveGrovePath("home")
	assert.Equal(t, "", result, "home should resolve to empty (already loaded)")
}

func TestResolveEffectiveGrovePath_Explicit(t *testing.T) {
	result := resolveEffectiveGrovePath("/some/path/.scion")
	assert.Equal(t, "/some/path/.scion", result)
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
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	os.Chdir(tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	versionedSettings := `schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(versionedSettings), 0644))

	assert.True(t, detectHierarchyFormat(""))
}

func TestDetectHierarchyFormat_Legacy(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	os.Chdir(tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	legacySettings := `active_profile: local
harnesses:
  gemini:
    image: test
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(legacySettings), 0644))

	assert.False(t, detectHierarchyFormat(""))
}

func TestDetectHierarchyFormat_NoFiles(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	os.Chdir(tmpDir)

	assert.False(t, detectHierarchyFormat(""))
}

func TestDetectHierarchyFormat_GroveVersioned(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	os.Chdir(tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	// Global is legacy, grove is versioned
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
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(versionedSettings), 0644))

	assert.True(t, detectHierarchyFormat(groveDir))
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
	assert.Equal(t, "profile_value", hc.Env["PROFILE_KEY"], "profile env should be merged")
	assert.Equal(t, "override_value", hc.Env["OVERRIDE_KEY"], "override env should be merged")
	assert.Len(t, hc.Volumes, 1, "profile volume should be appended")
	assert.Equal(t, "/mnt/vol", hc.Volumes[0].Target)
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
	tmux := true
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
				Tmux:    &tmux,
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
			Enabled:        true,
			Port:           9800,
			BrokerID:       "broker-123",
			BrokerName:     "my-broker",
			BrokerNickname: "nick",
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

	gc := ConvertV1ServerToGlobalConfig(v1)

	assert.Equal(t, "debug", gc.LogLevel)
	assert.Equal(t, "json", gc.LogFormat)
	assert.Equal(t, 9810, gc.Hub.Port)
	assert.Equal(t, "https://hub.example.com", gc.Hub.Endpoint)
	assert.Equal(t, true, gc.Hub.CORSEnabled)
	assert.Equal(t, 3600, gc.Hub.CORSMaxAge)
	assert.Equal(t, true, gc.RuntimeBroker.Enabled)
	assert.Equal(t, 9800, gc.RuntimeBroker.Port)
	assert.Equal(t, "broker-123", gc.RuntimeBroker.BrokerID)
	// BrokerName takes priority over BrokerNickname when both are set
	assert.Equal(t, "my-broker", gc.RuntimeBroker.BrokerName)
	assert.Equal(t, "https://hub.example.com", gc.RuntimeBroker.HubEndpoint)
	assert.Equal(t, "sqlite", gc.Database.Driver)
	assert.Equal(t, "/tmp/hub.db", gc.Database.URL)
	assert.Equal(t, true, gc.Auth.Enabled)
	assert.Equal(t, "dev-token", gc.Auth.Token)
	assert.Equal(t, "local", gc.Storage.Provider)
	assert.Equal(t, "/tmp/storage", gc.Storage.LocalPath)
	assert.Equal(t, "local", gc.Secrets.Backend)
}

func TestConvertV1ServerToGlobalConfig_Nil(t *testing.T) {
	gc := ConvertV1ServerToGlobalConfig(nil)
	assert.NotNil(t, gc)
	// Should be defaults
	assert.Equal(t, "info", gc.LogLevel)
}

func TestConvertGlobalToV1ServerConfig_RoundTrip(t *testing.T) {
	gc := DefaultGlobalConfig()
	gc.LogLevel = "debug"
	gc.Hub.Port = 9999
	gc.RuntimeBroker.Enabled = true
	gc.RuntimeBroker.BrokerID = "broker-abc"
	gc.RuntimeBroker.BrokerName = "test-broker"
	gc.Database.Driver = "sqlite"
	gc.Auth.Enabled = true
	gc.Auth.Token = "test-token"

	v1 := ConvertGlobalToV1ServerConfig(&gc)

	assert.Equal(t, "debug", v1.LogLevel)
	assert.Equal(t, 9999, v1.Hub.Port)
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
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

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
	assert.Equal(t, true, gc.RuntimeBroker.Enabled)
	assert.Equal(t, 8888, gc.RuntimeBroker.Port)
	assert.Equal(t, "test-broker-id", gc.RuntimeBroker.BrokerID)
	assert.Equal(t, "test-broker-nick", gc.RuntimeBroker.BrokerName)
	assert.Equal(t, true, gc.Auth.Enabled)
	assert.Equal(t, "test-dev-token", gc.Auth.Token)
}

func TestLoadGlobalConfig_SettingsYAMLPreferredOverServerYAML(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

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
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

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
	state, err := LoadGroveState(tmpDir)
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
        image: us-central1-docker.pkg.dev/ptone-misc/public-docker/scion-claude:latest
        user: scion
    codex:
        image: us-central1-docker.pkg.dev/ptone-misc/public-docker/scion-codex:latest
        user: scion
    gemini:
        image: us-central1-docker.pkg.dev/ptone-misc/public-docker/scion-gemini:latest
        user: scion
    opencode:
        image: us-central1-docker.pkg.dev/ptone-misc/public-docker/scion-opencode:latest
        user: scion
profiles:
    local:
        runtime: container
        tmux: true
        env:
            GIT_AUTHOR_EMAIL: ptone@google.com
            GIT_AUTHOR_NAME: Preston Holmes
            GIT_COMMITTER_EMAIL: ptone@google.com
            GIT_COMMITTER_NAME: Preston Holmes
        volumes:
            - source: ${GOPATH}/pkg
              target: /home/scion/go/pkg
            - source: /Users/ptone/Library/Caches/go-build
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
	require.NotNil(t, vs.Runtimes["container"].Tmux)
	assert.True(t, *vs.Runtimes["container"].Tmux)

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
	assert.Equal(t, "ptone@google.com", vs.Profiles["local"].Env["GIT_AUTHOR_EMAIL"])
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
                auth_selectedType: oauth-personal
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
	assert.Equal(t, "oauth-personal", vs.Profiles["local"].HarnessOverrides["gemini"].AuthSelectedType)

	// Also verify the raw YAML contains snake_case, not camelCase
	assert.Contains(t, string(newData), "auth_selected_type")
	assert.NotContains(t, string(newData), "auth_selectedType")
}

// --- UpdateSetting v1 format preservation tests ---

func TestUpdateSetting_PreservesV1Format(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	// Write a v1 versioned settings file
	v1Content := `schema_version: "1"
active_profile: local
hub:
  enabled: true
  endpoint: https://hub.example.com
  grove_id: original-grove-id
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(v1Content), 0644))

	// Call UpdateSetting with a key that would clobber the format in the old code
	err := UpdateSetting(groveDir, "grove_id", "new-grove-id", false)
	require.NoError(t, err)

	// Read back the file and verify it's still v1 format
	data, err := os.ReadFile(filepath.Join(groveDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(data)
	assert.Equal(t, "1", version, "file should still be v1 format after UpdateSetting")

	// Verify the field was updated
	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))
	assert.Equal(t, "new-grove-id", vs.Hub.GroveID)

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
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	v1Content := `schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(v1Content), 0644))

	err := UpdateSetting(groveDir, "hub.enabled", "true", false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(groveDir, "settings.yaml"))
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
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	v1Content := `schema_version: "1"
active_profile: local
hub:
  endpoint: https://hub.example.com
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(v1Content), 0644))

	// Update hub.brokerId — in v1 this maps to server.broker.broker_id
	err := UpdateSetting(groveDir, "hub.brokerId", "broker-123", false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(groveDir, "settings.yaml"))
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
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	v1Content := `schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(v1Content), 0644))

	err := UpdateSetting(groveDir, "hub.brokerToken", "token-xyz", false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(groveDir, "settings.yaml"))
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
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	v1Content := `schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(v1Content), 0644))

	// These keys are deprecated in v1 and should be silently skipped
	for _, key := range []string{"hub.token", "hub.apiKey", "hub.lastSyncedAt"} {
		err := UpdateSetting(groveDir, key, "some-value", false)
		assert.NoError(t, err, "deprecated key %s should not error", key)
	}

	// File should be unchanged (besides schema_version being preserved)
	data, err := os.ReadFile(filepath.Join(groveDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(data)
	assert.Equal(t, "1", version)
}

func TestUpdateSetting_V1MultipleUpdatesPreserveFormat(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	v1Content := `schema_version: "1"
active_profile: local
default_template: gemini
hub:
  enabled: true
  endpoint: https://hub.example.com
  grove_id: grove-1
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(v1Content), 0644))

	// Simulate what happens during hub operations: multiple sequential updates
	require.NoError(t, UpdateSetting(groveDir, "grove_id", "new-grove-id", false))
	require.NoError(t, UpdateSetting(groveDir, "hub.brokerId", "broker-abc", false))
	require.NoError(t, UpdateSetting(groveDir, "hub.brokerToken", "token-xyz", false))
	require.NoError(t, UpdateSetting(groveDir, "hub.enabled", "false", false))

	// Read final state
	data, err := os.ReadFile(filepath.Join(groveDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(data)
	assert.Equal(t, "1", version, "file should still be v1 after multiple updates")

	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))

	// Verify all updates took effect
	assert.Equal(t, "new-grove-id", vs.Hub.GroveID)
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

func TestUpdateSetting_LegacyFormatStillWorks(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	groveDir := filepath.Join(tmpDir, "my-grove", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	// Write legacy format (no schema_version)
	legacyContent := `active_profile: local
hub:
  endpoint: https://hub.example.com
  brokerId: old-broker
`
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(legacyContent), 0644))

	// UpdateSetting should use legacy path
	err := UpdateSetting(groveDir, "grove_id", "my-grove-id", false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(groveDir, "settings.yaml"))
	require.NoError(t, err)

	// Should remain legacy format (no schema_version)
	version, _ := DetectSettingsFormat(data)
	assert.Empty(t, version, "legacy file should remain legacy after UpdateSetting")

	// Verify the update
	assert.Contains(t, string(data), "grove_id: my-grove-id")
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
	assert.Equal(t, "test-grove", vs.Hub.GroveID)
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
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

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

// --- Helper ---

func boolPtr(b bool) *bool {
	return &b
}
