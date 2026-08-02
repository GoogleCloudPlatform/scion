package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadVersionedSettings_ProjectIDRemapping(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Test SCION_HUB_PROJECT_ID maps correctly to hub.project_id
	_ = os.Setenv("SCION_HUB_PROJECT_ID", "env-project-id")
	defer func() { _ = os.Unsetenv("SCION_HUB_PROJECT_ID") }()

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	require.NotNil(t, vs.Hub)
	// V1HubClientConfig.ProjectID has koanf:"project_id" tag,
	// so it should be populated from SCION_HUB_PROJECT_ID (mapped to hub.project_id).
	assert.Equal(t, "env-project-id", vs.Hub.ProjectID)
}

func TestLoadSingleFileVersioned_GroveIDBackwardCompat(t *testing.T) {
	// Verify that settings files using the legacy grove_id key under hub
	// are still read correctly after the struct tag migration to project_id.
	tmpDir := t.TempDir()

	settingsContent := `schema_version: "1"
hub:
  endpoint: "https://hub.example.com"
  grove_id: "legacy-grove-uuid-1234"
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(settingsContent), 0644))

	vs, err := LoadSingleFileVersioned(tmpDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "legacy-grove-uuid-1234", vs.Hub.ProjectID, "grove_id should be read into ProjectID for backward compatibility")
	assert.Equal(t, "https://hub.example.com", vs.Hub.Endpoint)
}

func TestLoadVersionedSettings_GroveIDBackwardCompat(t *testing.T) {
	// Verify that the koanf-based loader also handles grove_id in settings files.
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	settingsContent := `schema_version: "1"
hub:
  endpoint: "https://hub.example.com"
  grove_id: "legacy-grove-uuid-5678"
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "legacy-grove-uuid-5678", vs.Hub.ProjectID, "grove_id should be remapped to project_id in koanf loading")
}

func TestSaveVersionedSettings_WritesProjectID(t *testing.T) {
	// Verify that saving settings now writes project_id (not grove_id) in the YAML output.
	tmpDir := t.TempDir()

	vs := &VersionedSettings{
		SchemaVersion: "1",
		Hub: &V1HubClientConfig{
			Endpoint:  "https://hub.example.com",
			ProjectID: "my-project-id",
		},
	}

	require.NoError(t, SaveVersionedSettings(tmpDir, vs))

	data, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "project_id: my-project-id", "should write project_id, not grove_id")
	assert.NotContains(t, string(data), "grove_id", "should not write grove_id")
}

func TestUpdateVersionedSetting_SnakeCaseKeys(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.MkdirAll(tmpDir, 0755))

	// 1. Test hub.project_id
	err := UpdateVersionedSetting(tmpDir, "hub.project_id", "project-123")
	require.NoError(t, err)

	vs, err := LoadSingleFileVersioned(tmpDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "project-123", vs.Hub.ProjectID)

	// 2. Test hub.grove_id
	err = UpdateVersionedSetting(tmpDir, "hub.grove_id", "grove-456")
	require.NoError(t, err)

	vs, err = LoadSingleFileVersioned(tmpDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "grove-456", vs.Hub.ProjectID)

	// 3. Test GetVersionedSettingValue with snake_case
	val, err := GetVersionedSettingValue(vs, "hub.project_id")
	require.NoError(t, err)
	assert.Equal(t, "grove-456", val)

	val, err = GetVersionedSettingValue(vs, "hub.grove_id")
	require.NoError(t, err)
	assert.Equal(t, "grove-456", val)
}
