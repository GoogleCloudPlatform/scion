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
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Test SCION_HUB_PROJECT_ID maps correctly and remaps to grove_id
	os.Setenv("SCION_HUB_PROJECT_ID", "env-project-id")
	defer os.Unsetenv("SCION_HUB_PROJECT_ID")

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	require.NotNil(t, vs.Hub)
	// V1HubClientConfig.ProjectID has koanf:"grove_id" tag,
	// so it should be populated from SCION_HUB_PROJECT_ID (mapped to hub.project_id)
	// via our new remapping logic.
	assert.Equal(t, "env-project-id", vs.Hub.ProjectID)
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
