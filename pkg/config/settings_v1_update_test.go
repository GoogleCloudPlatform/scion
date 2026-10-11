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
	"gopkg.in/yaml.v3"
)

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
	err := UpdateSetting(projectDir, "project_id", "new-project-id", false)
	require.NoError(t, err)

	// Read back the file and verify it's still v1 format
	data, err := os.ReadFile(filepath.Join(projectDir, "settings.yaml"))
	require.NoError(t, err)

	version, _ := DetectSettingsFormat(data)
	assert.Equal(t, "1", version, "file should still be v1 format after UpdateSetting")

	// Verify the field was updated
	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))
	assert.Equal(t, "new-project-id", vs.Hub.ProjectID)

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
	require.NoError(t, UpdateSetting(projectDir, "project_id", "new-project-id", false))
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
	assert.Equal(t, "new-project-id", vs.Hub.ProjectID)
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
	err := UpdateSetting(projectDir, "project_id", "my-project-id", false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(projectDir, "settings.yaml"))
	require.NoError(t, err)

	// Should now be v1 format after auto-migration
	version, _ := DetectSettingsFormat(data)
	assert.Equal(t, "1", version, "legacy file should be migrated to v1 after UpdateSetting")

	// Verify the update was applied (struct tags now use project_id)
	assert.Contains(t, string(data), "project_id: my-project-id")

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
