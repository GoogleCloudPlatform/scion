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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
			ProjectID: "test-project",
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
	assert.Equal(t, "test-project", vs.Hub.ProjectID)
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
			ProjectID: "test-project",
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
	assert.Equal(t, "test-project", legacy.Hub.ProjectID)
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
	assert.Equal(t, legacyHC.Env["OVERRIDE_KEY"], versionedHC.Env["OVERRIDE_KEY"], "override env should match")
	assert.Equal(t, len(legacyHC.Volumes), len(versionedHC.Volumes), "volume count should match")
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
