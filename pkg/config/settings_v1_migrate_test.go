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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

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
project_id: github.com/example/project
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
